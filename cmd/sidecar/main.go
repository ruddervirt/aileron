// sidecar implements the KubeVirt onDefineDomain hook for aileron builds.
// It modifies the libvirt domain XML to:
//   - Replace OVMF firmware paths with custom EFI vars from PVC
//   - Inject a real floppy device (isa-fdc, fda) backed by /efi/floppy.img
//     so Windows Setup finds Autounattend.xml on A: before scanning CDROMs.
//     Build VMs use pc-i440fx (see internal/build/vm.go) because RHEL's
//     qemu-kvm gates isa-fdc off for q35.
//
// The binary is discovered by the KubeVirt sidecar-shim at /usr/bin/onDefineDomain
// and called with --vmi (JSON) and --domain (XML) flags.
package main

import (
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"libvirt.org/go/libvirtxml"
)

const (
	efiCodePath = "/var/run/efivars/OVMF_CODE.fd"
	// efiVarsTemplatePath is the blank OVMF vars file that libvirt copies
	// from on first boot when the writable NVRam doesn't yet exist.
	efiVarsTemplatePath = "/var/run/efivars/OVMF_VARS.fd"
	// efiVarsLivePath is the writable NVRam path. Placing it on the EFI PVC
	// (rather than KubeVirt's default ephemeral /var/run/kubevirt-private/...
	// path) makes boot entries written by the guest OS installer persist
	// across VM restarts and propagate into linked builds, which copy the
	// parent's EFI PVC contents wholesale.
	efiVarsLivePath = "/var/run/efivars/OVMF_VARS_LIVE.fd"

	// Floppy image lives on the EFI PVC.
	// In the sidecar container it's at /efivars (volumePath).
	// In the compute container it's at /var/run/efivars (sharedComputePath).
	floppySidecarPath = "/efivars/floppy.img"
	floppyComputePath = "/var/run/efivars/floppy.img"
)

func main() {
	var vmiJSON, domainXML string
	pflag.StringVar(&vmiJSON, "vmi", "", "VMI spec in JSON format")
	pflag.StringVar(&domainXML, "domain", "", "Domain spec in XML format")
	pflag.Parse()

	logger := log.New(os.Stderr, "aileron-sidecar: ", 0)

	if domainXML == "" {
		logger.Fatal("--domain is required")
	}

	result, err := onDefineDomain(logger, []byte(domainXML))
	if err != nil {
		logger.Fatalf("onDefineDomain failed: %v", err)
	}

	fmt.Print(result)
}

func onDefineDomain(logger *log.Logger, domainXML []byte) (string, error) {
	var domain libvirtxml.Domain
	if err := xml.Unmarshal(domainXML, &domain); err != nil {
		return "", fmt.Errorf("unmarshal domain XML: %w", err)
	}

	replaceEFIPaths(logger, &domain)

	// Only inject floppy if the image exists on the EFI PVC.
	if _, err := os.Stat(floppySidecarPath); err == nil {
		replaceFloppyDevice(logger, &domain)
	}

	out, err := xml.Marshal(domain)
	if err != nil {
		return "", fmt.Errorf("marshal domain XML: %w", err)
	}
	return string(out), nil
}

// replaceEFIPaths replaces the default OVMF firmware paths with the custom
// ones from the EFI PVC mounted at /var/run/efivars/. Both CODE and VARS
// must come from the same OVMF build to be compatible.
//
// The writable NVRam (the chardata content of <nvram>) is also redirected
// onto the PVC. KubeVirt's default writable path lives in launcher-pod
// ephemeral storage, so the boot entries an OS installer writes are lost
// when the VM stops — and a linked build inheriting the parent's PVC ends
// up with the unmodified blank template. Pointing the writable path at the
// PVC keeps those entries with the build artifact.
func replaceEFIPaths(logger *log.Logger, domain *libvirtxml.Domain) {
	if domain.OS == nil || domain.OS.Firmware == "" && domain.OS.Loader == nil {
		return
	}

	if domain.OS.Loader != nil && domain.OS.Loader.Path != "" {
		if strings.Contains(domain.OS.Loader.Path, "OVMF_CODE") {
			logger.Printf("replacing EFI code path: %s -> %s", domain.OS.Loader.Path, efiCodePath)
			domain.OS.Loader.Path = efiCodePath
		}
	}

	if domain.OS.NVRam != nil {
		if strings.Contains(domain.OS.NVRam.Template, "OVMF_VARS") {
			logger.Printf("replacing EFI vars template: %s -> %s", domain.OS.NVRam.Template, efiVarsTemplatePath)
			domain.OS.NVRam.Template = efiVarsTemplatePath
		}
		// Redirect the writable NVRam onto the PVC. On first boot the file
		// doesn't exist and libvirt initializes it by copying from Template;
		// on subsequent boots libvirt reuses the existing file as-is.
		if domain.OS.NVRam.NVRam != efiVarsLivePath {
			logger.Printf("replacing EFI vars writable path: %s -> %s", domain.OS.NVRam.NVRam, efiVarsLivePath)
			domain.OS.NVRam.NVRam = efiVarsLivePath
		}
	}
}

// replaceFloppyDevice injects the floppy image as a real isa-fdc floppy
// drive (fda). Windows Setup scans drive letters alphabetically for
// autounattend.xml and floppies always get A:, so this beats any
// autounattend bundled on the install CDROM (which lands on D:+). Q35's
// ICH9 LPC bridge exposes the ISA bus that isa-fdc plugs onto; libvirt
// auto-creates the <controller type='fdc'/> when it sees the floppy disk.
func replaceFloppyDevice(logger *log.Logger, domain *libvirtxml.Domain) {
	if domain.Devices == nil {
		domain.Devices = &libvirtxml.DomainDeviceList{}
	}

	// Check if already injected (hook may be called multiple times).
	for _, disk := range domain.Devices.Disks {
		if disk.Source != nil && disk.Source.File != nil &&
			strings.Contains(disk.Source.File.File, "floppy") {
			logger.Printf("floppy device already present, skipping")
			return
		}
	}

	logger.Printf("injecting floppy device fda (compute path: %s)", floppyComputePath)

	domain.Devices.Disks = append(domain.Devices.Disks, libvirtxml.DomainDisk{
		Device: "floppy",
		Driver: &libvirtxml.DomainDiskDriver{
			Name: "qemu",
			Type: "raw",
		},
		Source: &libvirtxml.DomainDiskSource{
			File: &libvirtxml.DomainDiskSourceFile{
				File: floppyComputePath,
			},
		},
		Target: &libvirtxml.DomainDiskTarget{
			Dev: "fda",
			Bus: "fdc",
		},
		Alias: &libvirtxml.DomainAlias{
			Name: "ua-floppy0",
		},
		ReadOnly: &libvirtxml.DomainDiskReadOnly{},
	})
}
