package main

import (
	"fmt"
	"github.com/pkg/errors"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/crio-lxc/cmd/internal"
	"github.com/rs/zerolog"
	"gopkg.in/lxc/go-lxc.v2"
)

// time format used for logger
const timeFormatLXCMillis = "20060102150405.000"

// The singelton that wraps the lxc.Container
var clxc crioLXC
var log zerolog.Logger

var errContainerNotExist = errors.New("container does not exist")
var errContainerExist = errors.New("container already exists")

type crioLXC struct {
	Container *lxc.Container

	Command string

	// [ global settings ]
	RuntimeRoot    string
	ContainerID    string
	LogFile        *os.File
	LogFilePath    string
	LogLevel       lxc.LogLevel
	LogLevelString string
	BackupDir      string
	Backup         bool
	BackupOnError  bool
	SystemdCgroup  bool
	MonitorCgroup  string
	StartCommand   string
	InitCommand    string
	HookCommand    string

	// feature gates
	Seccomp       bool
	Capabilities  bool
	Apparmor      bool
	CgroupDevices bool

	// create flags
	BundlePath    string
	SpecPath      string // BundlePath + "/config.json"
	PidFile       string
	ConsoleSocket string
	CreateTimeout time.Duration

	// start flags
	StartTimeout time.Duration
}

var version string

func versionString() string {
	return fmt.Sprintf("%s (%s) (lxc:%s)", version, runtime.Version(), lxc.Version())
}

// runtimePath builds an absolute filepath which is relative to the container runtime root.
func (c *crioLXC) runtimePath(subPath ...string) string {
	return filepath.Join(c.RuntimeRoot, c.ContainerID, filepath.Join(subPath...))
}

func (c *crioLXC) configFilePath() string {
	return c.runtimePath("config")
}

// loadContainer checks for the existence of the lxc config file.
// It returns an error if the config file does not exist.
func (c *crioLXC) loadContainer() error {
	_, err := os.Stat(c.configFilePath())
	if os.IsNotExist(err) {
		return errContainerNotExist
	}
	if err != nil {
		return errors.Wrap(err, "failed to access config file")
	}

	container, err := lxc.NewContainer(c.ContainerID, c.RuntimeRoot)
	if err != nil {
		return errors.Wrap(err, "failed to load container")
	}
	err = container.LoadConfigFile(c.configFilePath())
	if err != nil {
		return errors.Wrap(err, "failed to load config file")
	}
	c.Container = container
	return nil
}

// createContainer creates a new container.
// It must only be called once during the lifecycle of a container.
func (c *crioLXC) createContainer() error {
	// avoid creating a container
	if _, err := os.Stat(c.configFilePath()); err == nil {
		return errContainerExist
	}

	if err := os.MkdirAll(c.runtimePath(), 0700); err != nil {
		return errors.Wrap(err, "failed to create container dir")
	}

	// An empty tmpfile is created to ensure that createContainer can only succeed once.
	// The config file is atomically activated in saveConfig.
	f, err := os.OpenFile(c.runtimePath(".config"), os.O_EXCL|os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		return err
	}
	f.Close()

	container, err := lxc.NewContainer(c.ContainerID, c.RuntimeRoot)
	if err != nil {
		return err
	}
	c.Container = container
	return nil
}

// saveConfig creates and atomically enables the lxc config file.
// Any config changes via clxc.setConfigItem must be done
// before calling saveConfig.
func (c *crioLXC) saveConfig() error {
	// Allow it to be called once and only after createContainer.
	tmpFile := c.runtimePath(".config")
	if _, err := os.Stat(tmpFile); err != nil {
		return errors.Wrap(err, "failed to stat config tmpfile")
	}
	// Don't overwrite an existing config.
	cfgFile := c.configFilePath()
	if _, err := os.Stat(cfgFile); err == nil {
		return fmt.Errorf("config file %s already exists", cfgFile)
	}
	err := c.Container.SaveConfigFile(tmpFile)
	if err != nil {
		return errors.Wrapf(err, "failed to save config file to '%s'", tmpFile)
	}
	if err := os.Rename(tmpFile, cfgFile); err != nil {
		return errors.Wrap(err, "failed to rename config file")
	}
	log.Debug().Str("file", cfgFile).Msg("created lxc config file")
	return nil
}

// Release releases/closes allocated resources (lxc.Container, LogFile)
func (c crioLXC) release() error {
	if c.Container != nil {
		if err := c.Container.Release(); err != nil {
			log.Error().Err(err).Msg("failed to release container")
		}
	}
	if c.LogFile != nil {
		return c.LogFile.Close()
	}
	return nil
}

func supportsConfigItem(keys ...string) bool {
	for _, key := range keys {
		if !lxc.IsSupportedConfigItem(key) {
			log.Debug().Str("lxc.config", key).Msg("unsupported config item")
			return false
		}
	}
	return true
}

func (c *crioLXC) getConfigItem(key string) string {
	vals := c.Container.ConfigItem(key)
	if len(vals) > 0 {
		first := vals[0]
		// some lxc config values are set to '(null)' if unset
		// eg. lxc.cgroup.dir
		if first != "(null)" {
			return first
		}
	}
	return ""
}

func (c *crioLXC) setConfigItem(key, value string) error {
	err := c.Container.SetConfigItem(key, value)
	if err != nil {
		return errors.Wrap(err, "failed to set config item '%s=%s'")
	}
	log.Debug().Str("lxc.config", key).Str("val", value).Msg("set config item")
	return nil
}

func (c *crioLXC) configureLogging() error {
	logDir := filepath.Dir(c.LogFilePath)
	err := os.MkdirAll(logDir, 0750)
	if err != nil {
		return errors.Wrapf(err, "failed to create log file directory %s", logDir)
	}

	f, err := os.OpenFile(c.LogFilePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return errors.Wrapf(err, "failed to open log file %s", c.LogFilePath)
	}
	c.LogFile = f

	zerolog.TimestampFieldName = "t"
	zerolog.LevelFieldName = "l"
	zerolog.MessageFieldName = "m"
	zerolog.CallerFieldName = "c"
	zerolog.TimeFieldFormat = timeFormatLXCMillis

	zerolog.TimestampFunc = func() time.Time {
		return time.Now().UTC()
	}

	// NOTE It's not possible change the possition of the timestamp.
	// The ttimestamp is appended to the to the log output because it is dynamically rendered
	// see https://github.com/rs/zerolog/issues/109
	log = zerolog.New(f).With().Timestamp().Caller().Str("cmd", c.Command).Str("cid", c.ContainerID).Logger()
	zerolog.CallerMarshalFunc = func(file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	level, err := parseLogLevel(c.LogLevelString)
	if err != nil {
		log.Error().Err(err).Stringer("val", level).Msg("using fallback log-level")
	}
	c.LogLevel = level

	switch level {
	case lxc.TRACE:
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	case lxc.DEBUG:
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case lxc.INFO, lxc.NOTICE:
		// zerolog does not support a `notice` log-level notice
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case lxc.WARN:
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case lxc.ERROR:
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	}
	return nil
}

func parseLogLevel(s string) (lxc.LogLevel, error) {
	switch strings.ToLower(s) {
	case "trace":
		return lxc.TRACE, nil
	case "debug":
		return lxc.DEBUG, nil
	case "info":
		return lxc.INFO, nil
	case "notice":
		return lxc.NOTICE, nil
	case "warn":
		return lxc.WARN, nil
	case "error":
		return lxc.ERROR, nil
	default:
		return lxc.INFO, fmt.Errorf("invalid log-level %s", s)
	}
}

// BackupRuntimeResources creates a backup of the container runtime resources.
// It returns the path to the backup directory.
//
// The following resources are backed up:
// - all resources created by crio-lxc (lxc config, init script, device creation script ...)
// - lxc logfiles (if logging is setup per container)
// - the runtime spec
func (c *crioLXC) backupRuntimeResources() (backupDir string, err error) {
	backupDir = filepath.Join(c.BackupDir, c.ContainerID)
	err = os.MkdirAll(c.BackupDir, 0700)
	if err != nil {
		return "", errors.Wrap(err, "failed to create backup dir")
	}
	err = copyDir(c.runtimePath(), backupDir)
	if err != nil {
		return backupDir, errors.Wrap(err, "failed to copy lxc runtime directory")
	}
	// remove syncfifo because it is not of any use and blocks 'grep' within the backup directory.
	syncFifoPath := filepath.Join(backupDir, internal.SyncFifoPath)
	// #nosec
	err = os.Remove(syncFifoPath)
	if err != nil {
		log.Warn().Err(err).Str("file", syncFifoPath).Msg("failed to remove syncfifo from backup dir")
	}
	err = copyDir(c.SpecPath, backupDir)
	if err != nil {
		return backupDir, errors.Wrap(err, "failed to copy runtime spec to backup dir")
	}
	return backupDir, nil
}

// TODO avoid shellout
func copyDir(src, dst string) error {
	// #nosec
	cmd := exec.Command("cp", "-r", "-p", src, dst)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Errorf("%s: %s: %s", strings.Join(cmd.Args, " "), err, string(output))
	}
	return nil
}

// runtime states https://github.com/opencontainers/runtime-spec/blob/v1.0.2/runtime.md
const (
	// the container is being created (step 2 in the lifecycle)
	stateCreating = "creating"
	// the runtime has finished the create operation (after step 2 in the lifecycle),
	// and the container process has neither exited nor executed the user-specified program
	stateCreated = "created"
	// the container process has executed the user-specified program
	// but has not exited (after step 5 in the lifecycle)
	stateRunning = "running"
	// the container process has exited (step 7 in the lifecycle)
	stateStopped = "stopped"

	// crio-lxc-init is started but blocking at the syncfifo
	envStateCreated = "CRIO_LXC_STATE=" + stateCreated
)

func (c *crioLXC) isContainerStopped() bool {
	return c.Container.State() == lxc.STOPPED
}

// getContainerInitState returns the runtime state of the container.
// It is used to determine whether the container state is 'created' or 'running'.
// The init process environment contains #envStateCreated if the the container
// is created, but not yet running/started.
// This requires the proc filesystem to be mounted on the host.
func (c *crioLXC) getContainerState() (int, string, error) {
	if c.isContainerStopped() {
		return 0, stateStopped, nil
	}

	pid := c.Container.InitPid()
	if pid < 0 {
		return 0, stateCreating, nil
	}

	envFile := fmt.Sprintf("/proc/%d/environ", pid)
	// #nosec
	data, err := ioutil.ReadFile(envFile)
	if err != nil {
		return 0, stateStopped, errors.Wrapf(err, "failed to read init process environment %s", envFile)
	}

	environ := strings.Split(string(data), "\000")
	for _, env := range environ {
		if env == envStateCreated {
			return pid, stateCreated, nil
		}
	}
	return pid, stateRunning, nil
}

func (c *crioLXC) deleteContainer() error {
	if err := c.Container.Destroy(); err != nil {
		return errors.Wrap(err, "failed to destroy container")
	}

	// "Note that resources associated with the container,
	// but not created by this container, MUST NOT be deleted."
	// TODO - because we set rootfs.managed=0, Destroy() doesn't
	// delete the /var/lib/lxc/$containerID/config file:

	c.tryRemoveCgroups()

	return os.RemoveAll(c.runtimePath())
}

func (c *crioLXC) tryRemoveCgroups() {
	configItems := []string{"lxc.cgroup.dir", "lxc.cgroup.dir.container", "lxc.cgroup.dir.monitor"}
	for _, item := range configItems {
		dir := c.getConfigItem(item)
		if dir == "" {
			continue
		}
		err := tryRemoveAllCgroupDir(dir)
		if err != nil {
			log.Warn().Err(err).Str("lxc.config", item).Msg("failed to remove cgroup scope")
			continue
		}
		// try to remove outer directory, in case this is the POD that is deleted
		// FIXME crio should delete the kubepods slice
		outerSlice := filepath.Dir(dir)
		err = tryRemoveAllCgroupDir(outerSlice)
		if err != nil {
			log.Debug().Err(err).Str("file", outerSlice).Msg("failed to remove cgroup slice")
		}
	}
}
