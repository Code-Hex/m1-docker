package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Code-Hex/vz"
	"github.com/jessevdk/go-flags"
	"github.com/kr/pty"
	archiver "github.com/mholt/archiver/v3"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/term"
)

const (
	vmlinuz       = "https://cloud-images.ubuntu.com/releases/focal/release/unpacked/ubuntu-20.04-server-cloudimg-arm64-vmlinuz-generic"
	initrd        = "https://cloud-images.ubuntu.com/releases/focal/release/unpacked/ubuntu-20.04-server-cloudimg-arm64-initrd-generic"
	diskImg       = "https://cloud-images.ubuntu.com/releases/focal/release/ubuntu-20.04-server-cloudimg-arm64.tar.gz"
	diskImgTarget = "focal-server-cloudimg-arm64.img"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatalf("%+v", err)
	}
}

// Options struct for parse command line arguments
type Options struct {
	CommandLine string `short:"c" long:"command" default:"console=hvc0"`
	Setup       bool   `short:"s" long:"setup"`
}

func run(ctx context.Context, argv []string) error {
	var opts Options
	p := flags.NewParser(&opts, flags.PrintErrors)
	if _, err := p.ParseArgs(argv); err != nil {
		return err
	}

	if opts.Setup {
		eg, egCtx := errgroup.WithContext(ctx)
		eg.Go(func() error {
			log.Println("setup vmlinuz...")
			defer log.Println("done vmlinuz")
			return vmlinuzSetup(egCtx)
		})
		eg.Go(func() error {
			log.Println("setup initrd...")
			defer log.Println("done initrd")
			return initrdSetup(egCtx)
		})
		eg.Go(func() error {
			log.Println("setup disk image...")
			defer log.Println("done disk image")
			return diskImgSetup(egCtx)
		})
		if err := eg.Wait(); err != nil {
			return err
		}

		log.Println("extending disk image...")
		// 20GB
		if err := extendDiskImg(diskImgTarget, "20480"); err != nil {
			return errors.WithStack(err)
		}
		log.Println("done extend disk image")
	}

	if err := runVM(ctx, opts.CommandLine); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func runVM(ctx context.Context, cmdline string) error {
	bootLoader := vz.NewLinuxBootLoader(
		"vmlinuz",
		vz.WithCommandLine(cmdline),
		vz.WithInitrd("initrd"),
	)

	config := vz.NewVirtualMachineConfiguration(
		bootLoader,
		2,
		2*1024*1024*1024,
	)

	ptmx, tty, err := pty.Open()
	if err != nil {
		panic(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	inFd := int(os.Stdin.Fd())
	oldInState, err := term.MakeRaw(inFd)
	if err != nil {
		log.Fatal(err)
	}
	defer term.Restore(inFd, oldInState)

	if err := pty.InheritSize(os.Stdout, ptmx); err != nil {
		log.Fatalf("error resizing ptmx: %s", err)
	}

	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		log.Fatal(err)
	}

	t := term.NewTerminal(os.Stdout, "")
	if err := t.SetSize(width, height); err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			_, err := io.Copy(t, ptmx)
			if err != nil {
				if unixIsEAGAIN(err) {
					continue
				}
				log.Println("pty stdout err", err)
			}
			break
		}
	}()

	// console
	serialPortAttachment := vz.NewFileHandleSerialPortAttachment(os.Stdin, tty)
	consoleConfig := vz.NewVirtioConsoleDeviceSerialPortConfiguration(serialPortAttachment)
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{
		consoleConfig,
	})

	// network
	vz.NewFileHandleNetworkDeviceAttachment()
	natAttachment := vz.NewNATNetworkDeviceAttachment()
	networkConfig := vz.NewVirtioNetworkDeviceConfiguration(natAttachment)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{
		networkConfig,
	})

	// entropy
	entropyConfig := vz.NewVirtioEntropyDeviceConfiguration()
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{
		entropyConfig,
	})

	diskImageAttachment, err := vz.NewDiskImageStorageDeviceAttachment(
		diskImgTarget,
		false,
	)
	if err != nil {
		return errors.WithStack(err)
	}

	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{
		vz.NewVirtioBlockDeviceConfiguration(diskImageAttachment),
	})

	// traditional memory balloon device which allows for managing guest memory. (optional)
	config.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{
		vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration(),
	})

	// socket device (optional)
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{
		vz.NewVirtioSocketDeviceConfiguration(),
	})

	if _, err := config.Validate(); err != nil {
		return errors.WithStack(err)
	}

	vm := vz.NewVirtualMachine(config)

	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, os.Interrupt)

	go func(vm *vz.VirtualMachine) {
		for {
			select {
			case <-ctx.Done():
				stopped, err := vm.RequestStop()
				if err != nil {
					close(done)
					log.Fatal("RequestStop:", err)
				}
				log.Println("stopped:", stopped)
				close(done)
			case <-sig:
				stopped, err := vm.RequestStop()
				if err != nil {
					close(done)
					log.Fatal("RequestStop:", err)
				}
				log.Println("stopped:", stopped)
				close(done)
			case newState := <-vm.StateChangedNotify():
				if newState == vz.VirtualMachineStateStopped {
					close(done)
				}
				log.Println(
					"newState:", newState,
					"state:", vm.State(),
					"canStart:", vm.CanStart(),
					"canResume:", vm.CanResume(),
					"canPause:", vm.CanPause(),
					"canStopRequest:", vm.CanRequestStop(),
				)
			}
		}
	}(vm)

	vm.Start(func(err error) {
		log.Println("in start:", err)
	})

	select {
	case <-done:
	}
	return nil
}

func extendDiskImg(name, size string) error {
	cmd := exec.Command(
		"dd",
		"if=/dev/zero",
		fmt.Sprintf("of=%s", name),
		fmt.Sprintf("seek=%s", size),
		"bs=1024k",
		"count=0",
	)
	return cmd.Run()
}

func diskImgSetup(ctx context.Context) error {
	diskImgName := filepath.Base(diskImg)
	if err := downloadFile(ctx, diskImg, diskImgName); err != nil {
		return errors.WithStack(err)
	}
	gz := archiver.NewTarGz()
	// extracted as ./focal-server-cloudimg-arm64.img/focal-server-cloudimg-arm64.img
	if err := gz.Extract(diskImgName, diskImgTarget, diskImgTarget); err != nil {
		return errors.WithStack(err)
	}
	if err := os.Remove(diskImgName); err != nil {
		return errors.WithStack(err)
	}
	err := os.Rename(diskImgTarget, "folder")
	if err != nil {
		return errors.WithStack(err)
	}
	err = os.Rename("folder/"+diskImgTarget, diskImgTarget)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := os.Remove("folder"); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func initrdSetup(ctx context.Context) error {
	if err := downloadFile(ctx, initrd, "initrd"); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func vmlinuzSetup(ctx context.Context) error {
	vmlinuzName := filepath.Base(vmlinuz)
	if err := downloadFile(ctx, vmlinuz, vmlinuzName); err != nil {
		return errors.WithStack(err)
	}
	if err := unarchiveGZip(vmlinuzName, "vmlinuz"); err != nil {
		return errors.WithStack(err)
	}
	if err := os.Remove(vmlinuzName); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func downloadFile(ctx context.Context, url string, filepath string) error {
	f, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return err
	}
	return nil
}

func unarchiveGZip(src string, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	df, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer df.Close()

	gz := archiver.NewGz()
	if err := gz.Decompress(f, df); err != nil {
		return err
	}
	return nil
}

// unixIsEAGAIN reports whether err is a syscall.EAGAIN wrapped in a PathError.
// See golang.org/issue/9205
func unixIsEAGAIN(err error) bool {
	if pe, ok := err.(*os.PathError); ok {
		if errno, ok := pe.Err.(syscall.Errno); ok && errno == syscall.EAGAIN {
			return true
		}
	}
	return false
}
