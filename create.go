package lxcri

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/lxcri/pkg/specki"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// Create creates a single container instance from the given ContainerConfig.
// Create is the first runtime method to call within the lifecycle of a container.
// A created Container must be released with Container.Release after use.
// You should call Runtime.Delete to cleanup container runtime state, even
// if the Create returned with an error.
func (rt *Runtime) Create(ctx context.Context, cfg *ContainerConfig) (*Container, error) {
	if err := rt.checkConfig(cfg); err != nil {
		return nil, err
	}

	c := &Container{ContainerConfig: cfg}
	c.runtimeDir = filepath.Join(rt.Root, c.ContainerID)

	if cfg.Spec.Annotations == nil {
		cfg.Spec.Annotations = make(map[string]string)
	}
	cfg.Spec.Annotations["org.linuxcontainers.lxc.ConfigFile"] = c.RuntimePath("config")

	if err := c.create(); err != nil {
		return c, errorf("failed to create container: %w", err)
	}

	if err := configureContainer(rt, c); err != nil {
		return c, errorf("failed to configure container: %w", err)
	}

	cleanenv(c, true)

	// Serialize the modified spec.Spec separately, to make it available for
	// runtime hooks.
	specPath := c.RuntimePath(BundleConfigFile)
	err := specki.EncodeJSONFile(specPath, cfg.Spec, os.O_EXCL|os.O_CREATE, 0444)
	if err != nil {
		return c, err
	}

	if rt.BackupConfigDir != "" {
		err := os.MkdirAll(rt.BackupConfigDir, 0700)
		if err != nil {
			rt.Log.Warn().Err(err).Str("dir", rt.BackupConfigDir).Msg("failed to create backup dir")
		}
		specPath := filepath.Join(rt.BackupConfigDir, cfg.ContainerID+".config.json")
		err = specki.EncodeJSONFile(specPath, cfg.Spec, os.O_EXCL|os.O_CREATE, 0444)
		if err != nil {
			rt.Log.Warn().Err(err).Str("file", specPath).Msg("failed to backup spec")
		}
	}

	err = specki.EncodeJSONFile(c.RuntimePath("hooks.json"), cfg.Spec.Hooks, os.O_EXCL|os.O_CREATE, 0444)
	if err != nil {
		return c, err
	}
	state, err := c.State()
	if err != nil {
		return c, err
	}
	err = specki.EncodeJSONFile(c.RuntimePath("state.json"), state.SpecState, os.O_EXCL|os.O_CREATE, 0444)
	if err != nil {
		return c, err
	}

	if err := rt.runStartCmd(ctx, c); err != nil {
		return c, errorf("failed to run container process: %w", err)
	}
	return c, nil
}

func configureUserNamespace(rt *Runtime, c *Container) {
	if rt.usernsConfigured {
		namesp := c.Spec.Linux.Namespaces
		for i, n := range namesp {
			if n.Type == specs.UserNamespace {
				rt.Log.Warn().Msg("Preconfigured user namespace is removed from the namespace list.")
				c.Spec.Linux.Namespaces = append(namesp[0:i], namesp[i+1:]...)
			}
		}
		return
	}

	if isNamespaceEnabled(c.Spec, specs.UserNamespace) {
		return
	}

	enableUserNamespace := false

	if os.Getuid() != 0 {
		rt.Log.Warn().Msg("unprivileged runtime - enabling user namespace")
		enableUserNamespace = true
	}

	if c.Spec.Annotations["org.linuxcontainers.lxcri.userns"] != "" {
		rt.Log.Warn().Msg("org.linuxcontainers.lxcri.userns annotation - enabling user namespace")
		enableUserNamespace = true
	}

	if enableUserNamespace {
		// TODO load subuid and subgid ranges for container user

		//c.Spec.Process.User.UID = 100000
		//c.Spec.Process.User.GID = 100000

		c.Spec.Linux.UIDMappings = []specs.LinuxIDMapping{
			{ContainerID: 0, HostID: 100000, Size: 100},
		}
		c.Spec.Linux.GIDMappings = []specs.LinuxIDMapping{
			{ContainerID: 0, HostID: 100000, Size: 100},
		}

		c.Spec.Linux.Namespaces = append(c.Spec.Linux.Namespaces,
			specs.LinuxNamespace{Type: specs.UserNamespace})
	}
}

func bindMountDevices(rt *Runtime, c *Container) {
	// the mknod hook is run in which context (lxc.hook.mount) ?
	// --> must run in lxc.hook.pre-mount instead !!!

	// if runtime process is not privileged or CAP_MKNOD is not granted `man capabilities`
	// then bind mount devices instead.
	//if !rt.isPrivileged() || !rt.hasCapability("mknod") {
	//	rt.Log.Info().Msg("runtime does not have capability CAP_MKNOD")
	newMounts := make([]specs.Mount, 0, len(c.Spec.Mounts)+len(c.Spec.Linux.Devices))
	for _, m := range c.Spec.Mounts {
		if m.Destination == "/dev" {
			os.MkdirAll(filepath.Join(c.Spec.Root.Path, "/dev"), 0755)
			newMounts = append(newMounts,
				specs.Mount{
					Destination: m.Destination, Source: "tmpfs", Type: "tmpfs",
					Options: m.Options,
				},
			)
			rt.Log.Info().Msg("device files are bind mounted")
			for _, device := range c.Spec.Linux.Devices {
				newMounts = append(newMounts,
					specs.Mount{
						Destination: device.Path, Source: device.Path, Type: "bind",
						Options: []string{"bind"},
					},
				)
			}
			continue
		}
		newMounts = append(newMounts, m)
	}
	c.Spec.Mounts = newMounts
	c.Spec.Linux.Devices = nil
}

func configureContainer(rt *Runtime, c *Container) error {
	if err := c.SetLog(c.LogFile, c.LogLevel); err != nil {
		return errorf("failed to configure container log (file:%s level:%s): %w", c.LogFile, c.LogLevel, err)
	}

	if err := configureHostname(rt, c); err != nil {
		return err
	}

	if err := configureRootfs(rt, c); err != nil {
		return fmt.Errorf("failed to configure rootfs: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(c.Spec.Root.Path, "run"), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(c.Spec.Root.Path, ".lxcri"), 0755); err != nil {
		return err
	}

	configureUserNamespace(rt, c)

	/*
		c.Spec.Linux.UIDMappings = []specs.LinuxIDMapping{
			{ContainerID: 0, HostID: 100000, Size: 100000},
		}
		c.Spec.Linux.GIDMappings = []specs.LinuxIDMapping{
			{ContainerID: 0, HostID: 100000, Size: 100000},
		}
	*/

	if err := configureNamespaces(c); err != nil {
		return fmt.Errorf("failed to configure namespaces: %w", err)
	}

	if err := configureInit(rt, c); err != nil {
		return fmt.Errorf("failed to configure init: %w", err)
	}

	if c.Spec.Process.OOMScoreAdj != nil {
		if err := c.setConfigItem("lxc.proc.oom_score_adj", fmt.Sprintf("%d", *c.Spec.Process.OOMScoreAdj)); err != nil {
			return err
		}
	}

	if c.Spec.Process.NoNewPrivileges {
		if err := c.setConfigItem("lxc.no_new_privs", "1"); err != nil {
			return err
		}
	}

	if rt.Features.Apparmor {
		if err := configureApparmor(c); err != nil {
			return fmt.Errorf("failed to configure apparmor: %w", err)
		}
	} else {
		rt.Log.Warn().Msg("apparmor feature is disabled - profile is set to unconfined")
	}

	if rt.Features.Seccomp {
		if c.Spec.Linux.Seccomp != nil && len(c.Spec.Linux.Seccomp.Syscalls) > 0 {
			profilePath := c.RuntimePath("seccomp.conf")
			if err := writeSeccompProfile(profilePath, c.Spec.Linux.Seccomp); err != nil {
				return err
			}
			if err := c.setConfigItem("lxc.seccomp.profile", profilePath); err != nil {
				return err
			}
		}
	} else {
		rt.Log.Warn().Msg("seccomp feature is disabled - all system calls are allowed")
	}

	if rt.Features.Capabilities {
		if err := configureCapabilities(c); err != nil {
			return fmt.Errorf("failed to configure capabilities: %w", err)
		}
	} else {
		rt.Log.Warn().Msg("capabilities feature is disabled - container inherits privileges of the runtime process")
	}

	// make sure autodev is disabled
	if err := c.setConfigItem("lxc.autodev", "0"); err != nil {
		return err
	}

	// NOTE crio can add devices (through the config) but this does not work for privileged containers.
	// See https://github.com/cri-o/cri-o/blob/a705db4c6d04d7c14a4d59170a0ebb4b30850675/server/container_create_linux.go#L45
	// File an issue on cri-o (at least for support)
	if err := specki.AllowEssentialDevices(c.Spec); err != nil {
		return err
	}

	bindMountDevices(rt, c)

	if err := configureHooks(rt, c); err != nil {
		return err
	}

	if err := configureCgroup(rt, c); err != nil {
		return fmt.Errorf("failed to configure cgroups: %w", err)
	}

	for key, val := range c.Spec.Linux.Sysctl {
		if err := c.setConfigItem("lxc.sysctl."+key, val); err != nil {
			return err
		}
	}

	// `man lxc.container.conf`: "A resource with no explicitly configured limitation will be inherited
	// from the process starting up the container"
	seenLimits := make([]string, 0, len(c.Spec.Process.Rlimits))
	for _, limit := range c.Spec.Process.Rlimits {
		name := strings.TrimPrefix(strings.ToLower(limit.Type), "rlimit_")
		for _, seen := range seenLimits {
			if seen == name {
				return fmt.Errorf("duplicate resource limit %q", limit.Type)
			}
		}
		seenLimits = append(seenLimits, name)
		val := fmt.Sprintf("%d:%d", limit.Soft, limit.Hard)
		if err := c.setConfigItem("lxc.prlimit."+name, val); err != nil {
			return err
		}
	}

	if err := configureMounts(rt, c); err != nil {
		return fmt.Errorf("failed to configure mounts: %w", err)
	}

	if err := configureReadonlyPaths(c); err != nil {
		return fmt.Errorf("failed to configure read-only paths: %w", err)
	}
	return nil
}

func configureHostname(rt *Runtime, c *Container) error {
	if c.Spec.Hostname == "" {
		return nil
	}
	if err := c.setConfigItem("lxc.uts.name", c.Spec.Hostname); err != nil {
		return err
	}

	// Check if UTS namespace is shared, but not with the host.
	uts := getNamespace(c.Spec, specs.UTSNamespace)
	if uts == nil || uts.Path == "" {
		return nil
	}

	yes, err := isNamespaceSharedWithRuntime(uts)
	if err != nil {
		return errorf("failed to check if uts namespace is shared with host: %w", err)
	}
	if yes {
		return nil
	}

	// Set the hostname on shared UTS namespace, since liblxc doesn't do it.
	if err := setHostname(uts.Path, c.Spec.Hostname); err != nil {
		return fmt.Errorf("failed  to set hostname: %w", err)
	}
	return nil
}

func configureRootfs(rt *Runtime, c *Container) error {
	rootfs := c.Spec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(c.BundlePath, rootfs)
	}

	if os.Getuid() != 0 {
		if err := unix.Chmod(rootfs, 0777); err != nil {
			return err
		}
	}

	if err := c.setConfigItem("lxc.rootfs.path", rootfs); err != nil {
		return err
	}

	if err := c.setConfigItem("lxc.rootfs.mount", rootfs); err != nil {
		return err
	}

	if err := c.setConfigItem("lxc.rootfs.managed", "0"); err != nil {
		return err
	}

	// Resources not created by the container runtime MUST NOT be deleted by it.
	if err := c.setConfigItem("lxc.ephemeral", "0"); err != nil {
		return err
	}

	rootfsOptions := []string{}
	if c.Spec.Linux.RootfsPropagation != "" {
		rootfsOptions = append(rootfsOptions, c.Spec.Linux.RootfsPropagation)
	}
	if c.Spec.Root.Readonly {
		rootfsOptions = append(rootfsOptions, "ro")
	}
	if err := c.setConfigItem("lxc.rootfs.options", strings.Join(rootfsOptions, ",")); err != nil {
		return err
	}
	return nil
}

func configureReadonlyPaths(c *Container) error {
	rootmnt := c.getConfigItem("lxc.rootfs.mount")
	if rootmnt == "" {
		return fmt.Errorf("lxc.rootfs.mount unavailable")
	}
	for _, p := range c.Spec.Linux.ReadonlyPaths {
		mnt := fmt.Sprintf("%s %s %s %s", filepath.Join(rootmnt, p), strings.TrimPrefix(p, "/"), "bind", "bind,ro,optional")
		if err := c.setConfigItem("lxc.mount.entry", mnt); err != nil {
			return fmt.Errorf("failed to make path readonly: %w", err)
		}
	}
	return nil
}

func configureApparmor(c *Container) error {
	// The value *apparmor_profile*  from crio.conf is used if no profile is defined by the container.
	aaprofile := c.Spec.Process.ApparmorProfile
	if aaprofile == "" {
		aaprofile = "unconfined"
	}
	return c.setConfigItem("lxc.apparmor.profile", aaprofile)
}

// configureCapabilities configures the linux capabilities / privileges granted to the container processes.
// See `man lxc.container.conf` lxc.cap.drop and lxc.cap.keep for details.
// https://blog.container-solutions.com/linux-capabilities-in-practice
// https://blog.container-solutions.com/linux-capabilities-why-they-exist-and-how-they-work
func configureCapabilities(c *Container) error {
	keepCaps := "none"
	if c.Spec.Process.Capabilities != nil {
		var caps []string
		for _, c := range c.Spec.Process.Capabilities.Permitted {
			lcCapName := strings.TrimPrefix(strings.ToLower(c), "cap_")
			caps = append(caps, lcCapName)
		}
		if len(caps) > 0 {
			keepCaps = strings.Join(caps, " ")
		}
	}

	return c.setConfigItem("lxc.cap.keep", keepCaps)
}

// NOTE keep in sync with cmd/lxcri-hook#ociHooksAndState
func configureHooks(rt *Runtime, c *Container) error {

	//  prepend runtime OCI hooks to container hooks
	hooks := rt.Hooks

	if c.Spec.Hooks != nil {
		if len(c.Spec.Hooks.Prestart) > 0 {
			hooks.Prestart = append(hooks.Prestart, c.Spec.Hooks.Prestart...)
		}
		if len(c.Spec.Hooks.CreateRuntime) > 0 {
			hooks.CreateRuntime = append(hooks.CreateRuntime, c.Spec.Hooks.CreateRuntime...)
		}
		if len(c.Spec.Hooks.CreateContainer) > 0 {
			hooks.CreateContainer = append(hooks.CreateContainer, c.Spec.Hooks.CreateContainer...)
		}
		if len(c.Spec.Hooks.StartContainer) > 0 {
			hooks.StartContainer = append(hooks.StartContainer, c.Spec.Hooks.StartContainer...)
		}
		if len(c.Spec.Hooks.Poststart) > 0 {
			hooks.Poststart = append(hooks.Poststart, c.Spec.Hooks.Poststart...)
		}
		if len(c.Spec.Hooks.Poststop) > 0 {
			hooks.Poststop = append(hooks.Poststop, c.Spec.Hooks.Poststop...)
		}
	}

	c.Spec.Hooks = &hooks

	// pass context information as environment variables to hook scripts
	if err := c.setConfigItem("lxc.hook.version", "1"); err != nil {
		return err
	}

	if len(c.Spec.Hooks.Prestart) > 0 || len(c.Spec.Hooks.CreateRuntime) > 0 {
		if err := c.setConfigItem("lxc.hook.pre-mount", rt.libexec(ExecHook)); err != nil {
			return err
		}
	}
	if len(c.Spec.Hooks.CreateContainer) > 0 {
		if err := c.setConfigItem("lxc.hook.mount", rt.libexec(ExecHook)); err != nil {
			return err
		}
	}
	if len(c.Spec.Hooks.StartContainer) > 0 {
		if err := c.setConfigItem("lxc.hook.start", rt.libexec(ExecHook)); err != nil {
			return err
		}
	}
	return nil
}

// cleanenv removes duplicates from spec.Process.Env.
// If overwrite is false the first defined value takes precedence,
// if overwrite is true, the last defined value overwrites previously
// defined values.
func cleanenv(c *Container, overwrite bool) {
	env := c.Spec.Process.Env
	if len(env) < 2 {
		return
	}
	newEnv := make([]string, 0, len(env))
	var exist bool
	for _, kv := range env {
		newEnv, exist = specki.Setenv(newEnv, kv, overwrite)
		if exist {
			vals := strings.Split(kv, "=")
			c.Log.Warn().Msgf("duplicate environment variable %s (overwrite=%t)", vals[0], overwrite)
		}
	}
	c.Spec.Process.Env = newEnv
}
