package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
	"time"

	"github.com/lxc/lxcri"
	"github.com/lxc/lxcri/pkg/specki"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/urfave/cli/v2"
	"sigs.k8s.io/yaml"
)

var (
	version = "undefined"
)

type app struct {
	*lxcri.Runtime

	command     string
	containerID string
}

var clxc app

func (app *app) loadContainer(containerID string) (*lxcri.Container, error) {
	c, err := clxc.Load(containerID)
	if err != nil {
		return c, err
	}
	c.Log = app.Runtime.Log
	err = c.SetLog(app.LogConfig.ContainerLogFile, app.LogConfig.ContainerLogLevel)
	return c, err
}

func (app *app) releaseContainer(c *lxcri.Container) {
	if c == nil {
		return
	}
	if err := c.Release(); err != nil {
		app.Runtime.Log.Error().Msgf("failed to release container: %s", err)
	}
}

func main() {
	clxc.Runtime = lxcri.NewRuntime(os.Getuid() != 0)
	if err := clxc.Runtime.LoadConfig(""); err != nil {
		panic(err)
	}
	app := cli.NewApp()
	app.Name = "lxcri"
	app.Usage = "lxcri is a OCI compliant runtime wrapper for lxc"
	app.Version = version

	// Disable the default ExitErrHandler.
	// It will call os.Exit if a command returns an error that implements
	// the cli.ExitCoder interface. E.g an unwrapped error from os.Exec.
	app.ExitErrHandler = func(context *cli.Context, err error) {}
	app.Commands = []*cli.Command{
		stateCmd(),
		createCmd(),
		startCmd(),
		killCmd(),
		deleteCmd(),
		execCmd(),
		inspectCmd(),
		listCmd(),
		configCmd(),
	}

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "log-level",
			Usage:       "set the runtime (lxcri) log level (trace|debug|info|warn|error)",
			EnvVars:     []string{"LXCRI_LOG_LEVEL"},
			Value:       clxc.LogConfig.LogLevel,
			Destination: &clxc.LogConfig.LogLevel,
		},
		&cli.StringFlag{
			Name:        "log-file",
			Usage:       "set the runtime (lxcri) log file path",
			EnvVars:     []string{"LXCRI_LOG_FILE"},
			Value:       clxc.LogConfig.LogFile,
			Destination: &clxc.LogConfig.LogFile,
		},
		&cli.StringFlag{
			Name:        "log-timestamp",
			Usage:       "timestamp format for the runtime log (see golang time package), default matches liblxc timestamp",
			EnvVars:     []string{"LXCRI_LOG_TIMESTAMP"}, // e.g  '0102 15:04:05.000'
			Value:       clxc.LogConfig.Timestamp,
			Destination: &clxc.LogConfig.Timestamp,
		},
		&cli.StringFlag{
			Name:        "container-log-level",
			Usage:       "set the container (liblxc) log level (trace|debug|info|notice|warn|error|crit|alert|fatal)",
			EnvVars:     []string{"LXCRI_CONTAINER_LOG_LEVEL"},
			Value:       clxc.LogConfig.ContainerLogLevel,
			Destination: &clxc.LogConfig.ContainerLogLevel,
		},
		&cli.StringFlag{
			Name:        "container-log-file",
			Usage:       "set the container (liblxc) log file path",
			EnvVars:     []string{"LXCRI_CONTAINER_LOG_FILE"},
			Value:       clxc.LogConfig.ContainerLogFile,
			Destination: &clxc.LogConfig.ContainerLogFile,
		},
		&cli.BoolFlag{
			Name:        "log-console",
			Usage:       "write log output to stderr (defaults to true if fd 0 is a tty, --log-file and --container-log-file options are ignored)",
			Destination: &clxc.LogConfig.LogConsole,
			Value:       isTerminal(0),
		},
		&cli.StringFlag{
			Name:    "root",
			Usage:   "root directory for storage of container runtime state (tmpfs is recommended)",
			EnvVars: []string{"LXCRI_ROOT"},
			// exec permissions are not required because init is bind mounted into the root
			Value:       clxc.Root,
			Destination: &clxc.Root,
		},
		&cli.BoolFlag{
			Name:  "systemd-cgroup",
			Usage: "cgroup path in container spec is systemd encoded and must be expanded",
		},
		&cli.StringFlag{
			Name:        "monitor-cgroup",
			Usage:       "cgroup path for liblxc monitor process",
			EnvVars:     []string{"LXCRI_MONITOR_CGROUP"},
			Value:       clxc.MonitorCgroup,
			Destination: &clxc.MonitorCgroup,
		},
		&cli.StringFlag{
			Name:        "libexec",
			Usage:       "path to directory that contains the runtime executables",
			EnvVars:     []string{"LXCRI_LIBEXEC"},
			Value:       clxc.LibexecDir,
			Destination: &clxc.LibexecDir,
		},
		&cli.BoolFlag{
			Name:        "apparmor",
			Usage:       "set apparmor profile defined in container spec",
			EnvVars:     []string{"LXCRI_APPARMOR"},
			Value:       clxc.Features.Apparmor,
			Destination: &clxc.Features.Apparmor,
		},
		&cli.BoolFlag{
			Name:        "capabilities",
			Usage:       "keep capabilities defined in container spec",
			EnvVars:     []string{"LXCRI_CAPABILITIES"},
			Value:       clxc.Features.Capabilities,
			Destination: &clxc.Features.Capabilities,
		},
		&cli.BoolFlag{
			Name:        "cgroup-devices",
			Usage:       "allow only devices permitted by container spec",
			EnvVars:     []string{"LXCRI_CGROUP_DEVICES"},
			Value:       clxc.Features.CgroupDevices,
			Destination: &clxc.Features.CgroupDevices,
		},
		&cli.BoolFlag{
			Name:        "seccomp",
			Usage:       "Generate and apply seccomp profile for lxc from container spec",
			EnvVars:     []string{"LXCRI_SECCOMP"},
			Value:       clxc.Features.Seccomp,
			Destination: &clxc.Features.Seccomp,
		},
		&cli.UintFlag{
			Name:        "create-timeout",
			Usage:       "maximum duration in seconds for create to complete",
			EnvVars:     []string{"LXCRI_CREATE_TIMEOUT"},
			Value:       clxc.Timeouts.CreateTimeout,
			Destination: &clxc.Timeouts.CreateTimeout,
		},
		&cli.UintFlag{
			Name:        "start-timeout",
			Usage:       "maximum duration in seconds for start to complete",
			EnvVars:     []string{"LXCRI_START_TIMEOUT"},
			Value:       clxc.Timeouts.StartTimeout,
			Destination: &clxc.Timeouts.StartTimeout,
		},
		&cli.UintFlag{
			Name:        "kill-timeout",
			Usage:       "timeout for killing all processes in container cgroup",
			EnvVars:     []string{"LXCRI_KILL_TIMEOUT"},
			Value:       clxc.Timeouts.KillTimeout,
			Destination: &clxc.Timeouts.KillTimeout,
		},
		&cli.UintFlag{
			Name:        "delete-timeout",
			Usage:       "maximum duration in seconds for delete to complete",
			EnvVars:     []string{"LXCRI_DELETE_TIMEOUT"},
			Value:       clxc.Timeouts.DeleteTimeout,
			Destination: &clxc.Timeouts.DeleteTimeout,
		},
	}

	startTime := time.Now()

	app.CommandNotFound = func(ctx *cli.Context, cmd string) {
		fmt.Fprintf(os.Stderr, "undefined subcommand %q cmdline%s\n", cmd, os.Args)
	}
	// Disable the default error messages for cmdline errors.
	// By default the app/cmd help is printed to stdout, which produces garbage in cri-o log output.
	// Instead the cmdline is printed to stderr to identify cmdline interface errors.
	errUsage := func(context *cli.Context, err error, isSubcommand bool) error {
		fmt.Fprintf(os.Stderr, "usage error %s: %s\n", err, os.Args)
		return err
	}
	app.OnUsageError = errUsage

	app.Before = func(ctx *cli.Context) error {
		clxc.command = ctx.Args().Get(0)
		return nil
	}

	setupCmd := func(ctx *cli.Context) error {
		switch clxc.command {
		case "list":
			if err := clxc.ConfigureLogger(); err != nil {
				return err
			}
		case "config":
			// ConfigureLogger changes the logging configuration
			// if LogConsole is enabled.
			// The original configuration must be restored.
			logCfg := clxc.Runtime.LogConfig
			if err := clxc.ConfigureLogger(); err != nil {
				return err
			}
			clxc.Runtime.LogConfig = logCfg
		default:
			containerID := ctx.Args().Get(0)
			if len(containerID) == 0 {
				return fmt.Errorf("missing container ID")
			}
			clxc.containerID = containerID

			clxc.LogConfig.LogContext = map[string]string{
				"cmd": clxc.command,
				"cid": clxc.containerID,
			}
			if err := clxc.Init(); err != nil {
				return err
			}
		}

		if clxc.ConfigPath == "" {
			clxc.Log.Debug().Msgf("no config file loaded")
		} else {
			clxc.Log.Debug().Msgf("using config file %q", clxc.ConfigPath)
		}

		clxc.Log.Debug().Strs("args", os.Args).Msg("started with")
		return nil
	}

	for _, cmd := range app.Commands {
		cmd.Before = setupCmd
		cmd.OnUsageError = errUsage
	}

	err := app.Run(os.Args)

	cmdDuration := time.Since(startTime)

	if err != nil {
		clxc.Log.Error().Err(err).Dur("duration", cmdDuration).Msg("command failed")
		clxc.Release()
		// write diagnostics message to stderr for crio/kubelet
		fmt.Fprintf(os.Stderr, "lxcri://%s %s\n", clxc.containerID, err)

		// exit with exit status of executed command
		var errExec execError
		if errors.As(err, &errExec) {
			os.Exit(errExec.exitStatus())
		}
		os.Exit(1)
	}

	clxc.Log.Debug().Dur("duration", cmdDuration).Msg("command completed")
	if err := clxc.Release(); err != nil {
		println(err.Error())
		os.Exit(1)
	}
}

func createCmd() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "create a container from a bundle directory",
		ArgsUsage: "<containerID>",
		Action:    doCreate,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "bundle",
				Usage: "set bundle directory",
				Value: ".",
			},
			&cli.StringFlag{
				Name:  "console-socket",
				Usage: "send container pty master fd to this socket path",
			},
			&cli.StringFlag{
				Name:  "pid-file",
				Usage: "path to write container PID",
			},
			&cli.BoolFlag{
				Name:  "no-new-keyring",
				Usage: "unused -required by buildah",
			},
			&cli.UintFlag{
				Name:        "timeout",
				Usage:       "maximum duration in seconds for create to complete",
				EnvVars:     []string{"LXCRI_CREATE_TIMEOUT"},
				Value:       clxc.Timeouts.CreateTimeout,
				Destination: &clxc.Timeouts.CreateTimeout,
			},
		},
	}
}

func doCreate(ctxcli *cli.Context) error {
	cfg := lxcri.ContainerConfig{
		ContainerID:   clxc.containerID,
		BundlePath:    ctxcli.String("bundle"),
		ConsoleSocket: ctxcli.String("console-socket"),
		SystemdCgroup: ctxcli.Bool("systemd-cgroup"),
		Log:           clxc.Runtime.Log,
		LogFile:       clxc.LogConfig.ContainerLogFile,
		LogLevel:      clxc.LogConfig.ContainerLogLevel,
	}

	specPath := filepath.Join(cfg.BundlePath, lxcri.BundleConfigFile)
	spec, err := specki.LoadSpecJSON(specPath)
	if err != nil {
		return fmt.Errorf("failed to load container spec from bundle: %w", err)
	}
	cfg.Spec = spec
	pidFile := ctxcli.String("pid-file")

	timeout := time.Duration(clxc.Timeouts.CreateTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err = doCreateInternal(ctx, &cfg, pidFile)
	if err != nil {
		clxc.Log.Error().Msgf("failed to create container: %s", err)
		// Create a new context because create may fail with a timeout.
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(clxc.Timeouts.DeleteTimeout)*time.Second)
		defer cancel()
		if err := clxc.Delete(ctx, clxc.containerID, true); err != nil {
			clxc.Log.Error().Err(err).Msg("failed to destroy container")
		}
		return err
	}
	return nil
}

func doCreateInternal(ctx context.Context, cfg *lxcri.ContainerConfig, pidFile string) error {
	c, err := clxc.Create(ctx, cfg)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)

	if pidFile != "" {
		err := createPidFile(pidFile, c.Pid)
		if err != nil {
			return err
		}
	}
	return nil
}

func startCmd() *cli.Command {
	return &cli.Command{
		Name:   "start",
		Usage:  "starts a container",
		Action: doStart,
		ArgsUsage: `[containerID]

starts <containerID>
`,
		Flags: []cli.Flag{
			&cli.UintFlag{
				Name:        "timeout",
				Usage:       "maximum duration in seconds for start to complete",
				EnvVars:     []string{"LXCRI_START_TIMEOUT"},
				Value:       clxc.Timeouts.StartTimeout,
				Destination: &clxc.Timeouts.StartTimeout,
			},
		},
	}
}

func doStart(ctxcli *cli.Context) error {

	timeout := time.Duration(clxc.Timeouts.StartTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c, err := clxc.loadContainer(clxc.containerID)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)
	return clxc.Start(ctx, c)
}

func stateCmd() *cli.Command {
	return &cli.Command{
		Name:   "state",
		Usage:  "returns state of a container",
		Action: doState,
		ArgsUsage: `[containerID]

<containerID> is the ID of the container you want to know about.
`,
		Flags: []cli.Flag{},
	}
}

func doState(unused *cli.Context) error {
	c, err := clxc.loadContainer(clxc.containerID)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)
	state, err := c.State()
	if err != nil {
		return err
	}
	j, err := json.Marshal(state.SpecState)
	if err != nil {
		return fmt.Errorf("failed to marshal json: %w", err)
	}
	clxc.Log.Trace().RawJSON("state", j).Msg("container state")
	_, err = fmt.Fprint(os.Stdout, string(j))
	return err
}

func killCmd() *cli.Command {
	return &cli.Command{
		Name:   "kill",
		Usage:  "sends a signal to a container",
		Action: doKill,
		ArgsUsage: `[containerID] [signal]

<containerID> is the ID of the container to send a signal to
[signal] signal name or numerical value (e.g [9|kill|KILL|sigkill|SIGKILL])
`,
		Flags: []cli.Flag{
			&cli.UintFlag{
				Name:        "timeout",
				Usage:       "timeout for killing all processes in container cgroup",
				EnvVars:     []string{"LXCRI_KILL_TIMEOUT"},
				Value:       clxc.Timeouts.KillTimeout,
				Destination: &clxc.Timeouts.KillTimeout,
			},
		},
	}
}

func doKill(ctxcli *cli.Context) error {
	sig := ctxcli.Args().Get(1)
	signum := parseSignal(sig)
	if signum == 0 {
		return fmt.Errorf("invalid signal param %q", sig)
	}

	c, err := clxc.loadContainer(clxc.containerID)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)

	timeout := time.Duration(clxc.Timeouts.KillTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return clxc.Kill(ctx, c, signum)
}

func deleteCmd() *cli.Command {
	return &cli.Command{
		Name:   "delete",
		Usage:  "deletes a container",
		Action: doDelete,
		ArgsUsage: `[containerID]

<containerID> is the ID of the container to delete
`,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "force",
				Usage: "force deletion",
			},
			&cli.UintFlag{
				Name:        "timeout",
				Usage:       "maximum duration in seconds for delete to complete",
				EnvVars:     []string{"LXCRI_DELETE_TIMEOUT"},
				Value:       clxc.Timeouts.DeleteTimeout,
				Destination: &clxc.Timeouts.DeleteTimeout,
			},
		},
	}
}

func doDelete(ctxcli *cli.Context) error {
	timeout := time.Duration(clxc.Timeouts.DeleteTimeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := clxc.Delete(ctx, clxc.containerID, ctxcli.Bool("force"))
	// Deleting a non-existing container is a noop,
	// otherwise cri-o / kubelet log warnings about that.
	if err == lxcri.ErrNotExist {
		return nil
	}
	return err
}

func execCmd() *cli.Command {
	return &cli.Command{
		Name:      "exec",
		Usage:     "execute a new process in a running container",
		ArgsUsage: "<containerID> [COMMAND] [args...]",
		Action:    doExec,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "process",
				Aliases: []string{"p"},
				Usage:   "path to process json - cmd and args are ignored if set",
				Value:   "",
			},
			&cli.StringFlag{
				Name:  "pid-file",
				Usage: "file to write the process id to",
				Value: "",
			},
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "detach from the executed process",
			},
			&cli.BoolFlag{
				Name:  "cgroup",
				Usage: "run in container cgroup namespace",
			},
			&cli.BoolFlag{
				Name:  "ipc",
				Usage: "run in container IPC namespace",
			},
			&cli.BoolFlag{
				Name:  "mnt",
				Usage: "run in container mount namespace",
			},
			&cli.BoolFlag{
				Name:  "net",
				Usage: "run in container network namespace",
			},
			&cli.BoolFlag{
				Name:  "pid",
				Usage: "run in container PID namespace",
			},
			//&cli.BoolFlag{
			//	Name:  "time",
			//	Usage: "run in container time namespace",
			//	Value: true,
			//},
			&cli.BoolFlag{
				Name:  "user",
				Usage: "run in container user namespace",
			},
			&cli.BoolFlag{
				Name:  "uts",
				Usage: "run in container UTS namespace",
			},
		},
	}
}

type execError int

func (e execError) exitStatus() int {
	return int(e)
}

func (e execError) Error() string {
	// liblxc remaps execvp exit codes to shell exit codes.
	// FIXME This is undocumented behaviour lxc/src/lxc/attach.c:lxc_attach_run_command
	// https://github.com/lxc/go-lxc/blob/d1943fb48dc73ef5cbc0ef43ed585420f7b2eb3a/container.go#L1370
	// RunCommandStatus returns with exitCode 126 or 127 but without error, so it is not possible to determine
	// whether this is the exit code from the command itself (e.g a shell itself) or from liblxc exec.
	switch int(e) {
	case 126:
		return "can not execute file: file header not recognized"
	case 127:
		return "executable file not found in $PATH"
	default:
		return fmt.Sprintf("cmd execution failed with exit status %d", e.exitStatus())
	}
}

// loadSpecProcess calls ReadSpecProcessJSON if the given specProcessPath is not empty,
// otherwise it creates a new specs.Process from the given args.
// It's an error if both values are empty.
func loadSpecProcess(specProcessPath string, args []string) (*specs.Process, error) {
	if specProcessPath != "" {
		return specki.LoadSpecProcessJSON(specProcessPath)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("spec process path and args are empty")
	}
	return &specs.Process{Cwd: "/", Args: args}, nil
}

func doExec(ctxcli *cli.Context) error {
	var args []string
	if ctxcli.Args().Len() > 1 {
		args = ctxcli.Args().Slice()[1:]
	}

	pidFile := ctxcli.String("pid-file")
	detach := ctxcli.Bool("detach")

	if detach && pidFile == "" {
		clxc.Log.Warn().Msg("detaching process but pid-file value is unset")
	}

	procSpec, err := loadSpecProcess(ctxcli.String("process"), args)
	if err != nil {
		return err
	}

	c, err := clxc.loadContainer(clxc.containerID)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)

	opts := lxcri.ExecOptions{}

	if ctxcli.Bool("cgroup") {
		opts.Namespaces = append(opts.Namespaces, specs.CgroupNamespace)
	}
	if ctxcli.Bool("ipc") {
		opts.Namespaces = append(opts.Namespaces, specs.IPCNamespace)
	}
	if ctxcli.Bool("mnt") {
		opts.Namespaces = append(opts.Namespaces, specs.MountNamespace)
	}
	if ctxcli.Bool("net") {
		opts.Namespaces = append(opts.Namespaces, specs.NetworkNamespace)
	}
	if ctxcli.Bool("pid") {
		opts.Namespaces = append(opts.Namespaces, specs.PIDNamespace)
	}
	//if ctxcli.Bool("time") {
	//	opts.Namespaces = append(opts.Namespaces, specs.TimeNamespace)
	//}
	if ctxcli.Bool("user") {
		opts.Namespaces = append(opts.Namespaces, specs.UserNamespace)
	}
	if ctxcli.Bool("uts") {
		opts.Namespaces = append(opts.Namespaces, specs.UTSNamespace)
	}

	c.Log.Info().Str("cmd", procSpec.Args[0]).
		Uint32("uid", procSpec.User.UID).Uint32("gid", procSpec.User.GID).
		Uints32("groups", procSpec.User.AdditionalGids).
		Str("namespaces", fmt.Sprintf("%s", opts.Namespaces)).Msg("execute cmd")

	if detach {
		pid, err := c.ExecDetached(procSpec, &opts)
		if err != nil {
			return err
		}
		if pidFile != "" {
			return createPidFile(pidFile, pid)
		}
	} else {
		status, err := c.Exec(procSpec, &opts)
		if err != nil {
			return err
		}
		if status != 0 {
			return execError(status)
		}
	}
	return nil
}

func inspectCmd() *cli.Command {
	return &cli.Command{
		Name:   "inspect",
		Usage:  "display the status of one or more containers",
		Action: doInspect,
		ArgsUsage: `containerID [containerID...]

<containerID> [containerID...] list of IDs for container to inspect
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "template",
				Usage: "Use this go template to format the output.",
			},
		},
	}
}

func doInspect(ctxcli *cli.Context) (err error) {
	var t *template.Template
	tmpl := ctxcli.String("template")
	if tmpl != "" {
		t, err = template.New("inspect").Parse(tmpl)
		if err != nil {
			return err
		}
	}

	for _, id := range ctxcli.Args().Slice() {
		if err := inspectContainer(id, t); err != nil {
			return err
		}
	}
	return nil
}

func listCmd() *cli.Command {
	return &cli.Command{
		Name:   "list",
		Usage:  "list available containers",
		Action: doList,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "template",
				Usage: "Use this go template to format the output.",
				// e.g `{{ printf "%s %s\n" .Container.ContainerID .State.ContainerState }}`,
			},
		},
	}
}

func doList(ctxcli *cli.Context) (err error) {
	tmpl := ctxcli.String("template")
	var t *template.Template
	if tmpl != "" {
		t, err = template.New("list").Parse(tmpl)
		if err != nil {
			return err
		}
	}

	all, err := clxc.List()
	if err != nil {
		return err
	}

	for _, id := range all {
		if t == nil {
			fmt.Println(id)
		} else {
			err := inspectContainer(id, t)
			if err != nil && !errors.Is(err, lxcri.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func inspectContainer(id string, t *template.Template) error {
	c, err := clxc.loadContainer(id)
	if err != nil {
		return err
	}
	defer clxc.releaseContainer(c)
	state, err := c.State()
	if err != nil {
		return fmt.Errorf("failed ot get container state: %w", err)
	}

	info := struct {
		Spec      *specs.Spec
		Container *lxcri.Container
		State     *lxcri.State
	}{
		Spec:      c.Spec,
		Container: c,
		State:     state,
	}

	if t != nil {
		return t.Execute(os.Stdout, info)
	}

	// avoid duplicate output
	c.Spec = nil
	state.SpecState.Annotations = nil

	j, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal json: %w", err)
	}
	_, err = fmt.Fprint(os.Stdout, string(j))
	return err
}

func configCmd() *cli.Command {
	return &cli.Command{
		Name:   "config",
		Usage:  "Output a config file for lxcri. Global options modify the output.",
		Action: doConfig,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "out",
				Usage: "write config to file",
			},
			&cli.BoolFlag{
				Name:  "update",
				Usage: "update the current config file (--out is ignored)",
			},
			&cli.BoolFlag{
				Name:  "default",
				Usage: "use default config file",
			},
			&cli.BoolFlag{
				Name:  "user",
				Usage: "use non-root configuration for default",
				Value: os.Getuid() != 0,
			},
			&cli.BoolFlag{
				Name:  "quiet",
				Usage: "do not print config to stdout",
			},
		},
	}
}

func doConfig(ctxcli *cli.Context) error {
	// generate yaml
	rt := clxc.Runtime
	if ctxcli.Bool("default") {
		rt = lxcri.NewRuntime(ctxcli.Bool("user"))
	}

	data, err := yaml.Marshal(rt)
	if err != nil {
		return err
	}
	if !ctxcli.Bool("quiet") {
		fmt.Printf("---\n%s---\n", string(data))
	}

	out := ctxcli.String("out")
	if ctxcli.Bool("update") {
		out = clxc.Runtime.ConfigPath
	}
	if out == "" {
		fmt.Printf("No output file defined.\n")
		return nil
	}
	fmt.Printf("Writing to file %s\n", out)
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return fmt.Errorf("failed to create config file parent directory: %w", err)
	}
	return os.WriteFile(out, data, 0644)
}
