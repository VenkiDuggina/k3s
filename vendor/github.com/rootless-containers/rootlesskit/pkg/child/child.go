package child

import (
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/rootless-containers/rootlesskit/pkg/common"
	"github.com/rootless-containers/rootlesskit/pkg/copyup"
	"github.com/rootless-containers/rootlesskit/pkg/msgutil"
	"github.com/rootless-containers/rootlesskit/pkg/network"
	"github.com/rootless-containers/rootlesskit/pkg/port"
)

func createCmd(targetCmd []string) (*exec.Cmd, error) {
	var args []string
	if len(targetCmd) > 1 {
		args = targetCmd[1:]
	}
	cmd := exec.Command(targetCmd[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	return cmd, nil
}

// mountSysfs is needed for mounting /sys/class/net
// when netns is unshared.
func mountSysfs() error {
	tmp, err := ioutil.TempDir("/tmp", "rksys")
	if err != nil {
		return errors.Wrap(err, "creating a directory under /tmp")
	}
	defer os.RemoveAll(tmp)
	cmds := [][]string{{"mount", "--rbind", "/sys/fs/cgroup", tmp}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	cmds = [][]string{{"mount", "-t", "sysfs", "none", "/sys"}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		// when the sysfs in the parent namespace is RO,
		// we can't mount RW sysfs even in the child namespace.
		// https://github.com/rootless-containers/rootlesskit/pull/23#issuecomment-429292632
		// https://github.com/torvalds/linux/blob/9f203e2f2f065cd74553e6474f0ae3675f39fb0f/fs/namespace.c#L3326-L3328
		cmdsRo := [][]string{{"mount", "-t", "sysfs", "-o", "ro", "none", "/sys"}}
		logrus.Warnf("failed to mount sysfs (%v), falling back to read-only mount (%v): %v",
			cmds, cmdsRo, err)
		if err := common.Execs(os.Stderr, os.Environ(), cmdsRo); err != nil {
			// when /sys/firmware is masked, even RO sysfs can't be mounted
			logrus.Warnf("failed to mount sysfs (%v): %v", cmdsRo, err)
		}
	}
	cmds = [][]string{{"mount", "-n", "--move", tmp, "/sys/fs/cgroup"}}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func activateLoopback() error {
	cmds := [][]string{
		{"ip", "link", "set", "lo", "up"},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func activateTap(tap, ip string, netmask int, gateway string, mtu int) error {
	cmds := [][]string{
		{"ip", "link", "set", tap, "up"},
		{"ip", "link", "set", "dev", tap, "mtu", strconv.Itoa(mtu)},
		{"ip", "addr", "add", ip + "/" + strconv.Itoa(netmask), "dev", tap},
		{"ip", "route", "add", "default", "via", gateway, "dev", tap},
	}
	if err := common.Execs(os.Stderr, os.Environ(), cmds); err != nil {
		return errors.Wrapf(err, "executing %v", cmds)
	}
	return nil
}

func setupCopyDir(driver copyup.ChildDriver, dirs []string) (bool, error) {
	if driver != nil {
		etcWasCopied := false
		copied, err := driver.CopyUp(dirs)
		for _, d := range copied {
			if d == "/etc" {
				etcWasCopied = true
				break
			}
		}
		return etcWasCopied, err
	}
	if len(dirs) != 0 {
		return false, errors.New("copy-up driver is not specified")
	}
	return false, nil
}

func setupNet(msg common.Message, etcWasCopied bool, driver network.ChildDriver) error {
	// HostNetwork
	if driver == nil {
		return nil
	}
	// for /sys/class/net
	if err := mountSysfs(); err != nil {
		return err
	}
	if err := activateLoopback(); err != nil {
		return err
	}
	tap, err := driver.ConfigureTap(msg.Network)
	if err != nil {
		return err
	}
	if err := activateTap(tap, msg.Network.IP, msg.Network.Netmask, msg.Network.Gateway, msg.Network.MTU); err != nil {
		return err
	}
	if etcWasCopied {
		if err := writeResolvConf(msg.Network.DNS); err != nil {
			return err
		}
		if err := writeEtcHosts(); err != nil {
			return err
		}
	} else {
		logrus.Warn("Mounting /etc/resolv.conf without copying-up /etc. " +
			"Note that /etc/resolv.conf in the namespace will be unmounted when it is recreated on the host. " +
			"Unless /etc/resolv.conf is statically configured, copying-up /etc is highly recommended. " +
			"Please refer to RootlessKit documentation for further information.")
		if err := mountResolvConf(msg.StateDir, msg.Network.DNS); err != nil {
			return err
		}
		if err := mountEtcHosts(msg.StateDir); err != nil {
			return err
		}
	}
	return nil
}

type Opt struct {
	PipeFDEnvKey  string              // needs to be set
	TargetCmd     []string            // needs to be set
	NetworkDriver network.ChildDriver // nil for HostNetwork
	CopyUpDriver  copyup.ChildDriver  // cannot be nil if len(CopyUpDirs) != 0
	CopyUpDirs    []string
	PortDriver    port.ChildDriver
}

func Child(opt Opt) error {
	if opt.PipeFDEnvKey == "" {
		return errors.New("pipe FD env key is not set")
	}
	pipeFDStr := os.Getenv(opt.PipeFDEnvKey)
	if pipeFDStr == "" {
		return errors.Errorf("%s is not set", opt.PipeFDEnvKey)
	}
	pipeFD, err := strconv.Atoi(pipeFDStr)
	if err != nil {
		return errors.Wrapf(err, "unexpected fd value: %s", pipeFDStr)
	}
	pipeR := os.NewFile(uintptr(pipeFD), "")
	var msg common.Message
	if _, err := msgutil.UnmarshalFromReader(pipeR, &msg); err != nil {
		return errors.Wrapf(err, "parsing message from fd %d", pipeFD)
	}
	logrus.Debugf("child: got msg from parent: %+v", msg)
	if msg.Stage == 0 {
		// the parent has configured the child's uid_map and gid_map, but the child doesn't have caps here.
		// so we exec the child again to obtain caps.
		// PID should be kept.
		if err = syscall.Exec("/proc/self/exe", os.Args, os.Environ()); err != nil {
			return err
		}
		panic("should not reach here")
	}
	if msg.Stage != 1 {
		return errors.Errorf("expected stage 1, got stage %d", msg.Stage)
	}
	os.Unsetenv(opt.PipeFDEnvKey)
	if err := pipeR.Close(); err != nil {
		return errors.Wrapf(err, "failed to close fd %d", pipeFD)
	}
	if msg.StateDir == "" {
		return errors.New("got empty StateDir")
	}
	etcWasCopied, err := setupCopyDir(opt.CopyUpDriver, opt.CopyUpDirs)
	if err != nil {
		return err
	}
	if err := setupNet(msg, etcWasCopied, opt.NetworkDriver); err != nil {
		return err
	}
	portQuitCh := make(chan struct{})
	portErrCh := make(chan error)
	if opt.PortDriver != nil {
		go func() {
			portErrCh <- opt.PortDriver.RunChildDriver(msg.Port.Opaque, portQuitCh)
		}()
	}

	cmd, err := createCmd(opt.TargetCmd)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "command %v exited", opt.TargetCmd)
	}
	if opt.PortDriver != nil {
		portQuitCh <- struct{}{}
		return <-portErrCh
	}
	return nil
}
