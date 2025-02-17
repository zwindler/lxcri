package lxcri

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/lxcri/pkg/specki"
	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func createFifo(dst string, mode uint32) error {
	if err := unix.Mkfifo(dst, mode); err != nil {
		return errorf("mkfifo dst:%s failed: %w", dst, err)
	}
	// lxcri-init must be able to write to the fifo.
	// Init process UID/GID can be different from runtime process UID/GID
	// liblxc changes the owner of the runtime directory to the effective container UID.
	// access to the files is protected by the runtimeDir
	// because umask (0022) affects unix.Mkfifo, a separate chmod is required
	// FIXME if container UID equals os.GetUID() and spec.
	if err := unix.Chmod(dst, mode); err != nil {
		return errorf("chmod mkfifo failed: %w", err)
	}
	return nil
}

// runAsRuntimeUser returns true if container process is started as runtime user.
func runAsRuntimeUser(spec *specs.Spec) bool {
	puid := specki.UnmapContainerID(spec.Process.User.UID, spec.Linux.UIDMappings)
	return puid == uint32(os.Getuid())
}

func configureInit(rt *Runtime, c *Container) error {
	initDir := "/.lxcri"

	c.Spec.Mounts = append(c.Spec.Mounts, specs.Mount{
		Source:      c.RuntimePath(),
		Destination: strings.TrimLeft(initDir, "/"),
		Type:        "bind",
		//Options:     []string{"rslave", "bind", "ro", "nodev", "nosuid", "create=dir"},
		Options: []string{"bind", "ro", "nodev", "nosuid", "create=dir"},
	})

	if err := c.setConfigItem("lxc.init.cwd", initDir); err != nil {
		return err
	}

	if runAsRuntimeUser(c.Spec) {
		if err := createFifo(c.syncFifoPath(), 0600); err != nil {
			return fmt.Errorf("failed to create sync fifo: %w", err)
		}
	} else {
		if err := createFifo(c.syncFifoPath(), 0666); err != nil {
			return fmt.Errorf("failed to create sync fifo: %w", err)
		}
	}

	if err := configureInitUser(rt, c); err != nil {
		return err
	}

	// bind mount lxcri-init into the container
	initCmdPath := c.RuntimePath("lxcri-init")
	err := touchFile(initCmdPath, 0)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", initCmdPath, err)
	}
	initCmd := filepath.Join(initDir, "lxcri-init")
	c.Spec.Mounts = append(c.Spec.Mounts, specs.Mount{
		Source:      rt.libexec(ExecInit),
		Destination: strings.TrimLeft(initCmd, "/"),
		Type:        "bind",
		//Options:     []string{"slave", "bind", "ro", "nosuid"},
		Options: []string{"bind", "ro", "nosuid"},
	})
	return c.setConfigItem("lxc.init.cmd", initCmd)
}

func touchFile(filePath string, perm os.FileMode) error {
	// #nosec
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDONLY, perm)
	if err == nil {
		return f.Close()
	}
	return err
}

func configureInitUser(rt *Runtime, c *Container) error {
	if !rt.usernsConfigured {
		for _, m := range c.Spec.Linux.UIDMappings {
			if err := c.setConfigItem("lxc.idmap", fmt.Sprintf("u %d %d %d", m.ContainerID, m.HostID, m.Size)); err != nil {
				return err
			}
		}
		for _, m := range c.Spec.Linux.GIDMappings {
			if err := c.setConfigItem("lxc.idmap", fmt.Sprintf("g %d %d %d", m.ContainerID, m.HostID, m.Size)); err != nil {
				return err
			}
		}
	}

	if err := c.setConfigItem("lxc.init.uid", fmt.Sprintf("%d", c.Spec.Process.User.UID)); err != nil {
		return err
	}
	if err := c.setConfigItem("lxc.init.gid", fmt.Sprintf("%d", c.Spec.Process.User.GID)); err != nil {
		return err
	}

	if len(c.Spec.Process.User.AdditionalGids) > 0 && c.supportsConfigItem("lxc.init.groups") {
		var b strings.Builder
		for i, gid := range c.Spec.Process.User.AdditionalGids {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%d", gid)
		}
		if err := c.setConfigItem("lxc.init.groups", b.String()); err != nil {
			return err
		}
	}
	return nil
}
