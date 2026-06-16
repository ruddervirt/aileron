package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"log"
	"strings"
	"testing"

	"libvirt.org/go/libvirtxml"
)

// kubevirtEFIDomainXML mirrors the libvirt domain XML KubeVirt emits for
// an EFI-enabled VM: the writable NVRam (chardata) defaults to the
// launcher pod's ephemeral nvram dir, which is what we need to redirect.
var kubevirtEFIDomainXML = fmt.Sprintf(`<domain type='kvm'>
  <name>vm-test</name>
  <os>
    <type arch='x86_64' machine='pc-q35-rhel9.8.0'>hvm</type>
    <loader readonly='yes' secure='no' type='pflash' format='raw'>%s</loader>
    <nvram template='%s' format='raw'>%s</nvram>
    <boot dev='hd'/>
  </os>
</domain>`,
	"/usr/share/OVMF/OVMF_CODE.fd",
	"/usr/share/OVMF/OVMF_VARS.fd",
	"/var/run/kubevirt-private/libvirt/qemu/nvram/vm-test_VARS.fd",
)

func TestOnDefineDomainRedirectsNVRamToPVC(t *testing.T) {
	logger := log.New(&bytes.Buffer{}, "", 0)

	out, err := onDefineDomain(logger, []byte(kubevirtEFIDomainXML))
	if err != nil {
		t.Fatalf("onDefineDomain: %v", err)
	}

	// Template path must be redirected to the PVC.
	if !strings.Contains(out, `template="/var/run/efivars/OVMF_VARS.fd"`) {
		t.Errorf("template path not redirected to PVC; got:\n%s", out)
	}

	// Writable NVRam (chardata) must be redirected to the PVC, not left
	// pointing at KubeVirt's ephemeral nvram dir. This is the fix for
	// boot entries failing to persist into linked builds.
	if !strings.Contains(out, `>/var/run/efivars/OVMF_VARS_LIVE.fd</nvram>`) {
		t.Errorf("writable NVRam path not redirected to PVC; got:\n%s", out)
	}
	if strings.Contains(out, "/var/run/kubevirt-private") {
		t.Errorf("writable NVRam still points at ephemeral kubevirt-private dir; got:\n%s", out)
	}

	// Loader (firmware code) must point at the PVC copy.
	if !strings.Contains(out, `>/var/run/efivars/OVMF_CODE.fd</loader>`) {
		t.Errorf("loader path not redirected to PVC; got:\n%s", out)
	}
}

func TestOnDefineDomainIdempotent(t *testing.T) {
	// Hooks may be invoked multiple times; rewriting an already-redirected
	// XML should be a no-op (no double-rewrite, no corruption).
	logger := log.New(&bytes.Buffer{}, "", 0)

	first, err := onDefineDomain(logger, []byte(kubevirtEFIDomainXML))
	if err != nil {
		t.Fatalf("first onDefineDomain: %v", err)
	}
	second, err := onDefineDomain(logger, []byte(first))
	if err != nil {
		t.Fatalf("second onDefineDomain: %v", err)
	}
	if first != second {
		t.Errorf("rewrite is not idempotent;\nfirst:\n%s\n\nsecond:\n%s", first, second)
	}
}

func TestReplaceFloppyDeviceInjectsFDC(t *testing.T) {
	logger := log.New(&bytes.Buffer{}, "", 0)
	var domain libvirtxml.Domain
	if err := xml.Unmarshal([]byte(kubevirtEFIDomainXML), &domain); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	replaceFloppyDevice(logger, &domain)
	out, err := xml.Marshal(domain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(out)
	for _, want := range []string{
		`device="floppy"`,
		`<source file="/var/run/efivars/floppy.img"`,
		`dev="fda"`,
		`bus="fdc"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in domain XML; got:\n%s", want, got)
		}
	}
	// Must NOT inject as CDROM/SATA — that's the old broken path.
	if strings.Contains(got, `device="cdrom"`) && strings.Contains(got, `bus="sata"`) {
		t.Errorf("floppy injected as SATA cdrom instead of fdc; got:\n%s", got)
	}
}

func TestReplaceFloppyDeviceIdempotent(t *testing.T) {
	logger := log.New(&bytes.Buffer{}, "", 0)
	var domain libvirtxml.Domain
	if err := xml.Unmarshal([]byte(kubevirtEFIDomainXML), &domain); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	replaceFloppyDevice(logger, &domain)
	first := len(domain.Devices.Disks)
	replaceFloppyDevice(logger, &domain)
	second := len(domain.Devices.Disks)

	if first != second {
		t.Errorf("floppy injected twice: %d -> %d disks", first, second)
	}
}

func TestOnDefineDomainNoEFI(t *testing.T) {
	// BIOS VMs (no <loader>/<nvram>) must pass through unchanged for
	// the EFI portion. We don't assert exact equality because the marshaler
	// may normalize whitespace, but we do assert no NVRam paths sneak in.
	logger := log.New(&bytes.Buffer{}, "", 0)

	const biosXML = `<domain type='kvm'>
  <name>vm-bios</name>
  <os>
    <type arch='x86_64' machine='pc-q35-rhel9.8.0'>hvm</type>
    <boot dev='hd'/>
  </os>
</domain>`

	out, err := onDefineDomain(logger, []byte(biosXML))
	if err != nil {
		t.Fatalf("onDefineDomain: %v", err)
	}
	if strings.Contains(out, "OVMF") || strings.Contains(out, "efivars") {
		t.Errorf("BIOS VM picked up EFI paths; got:\n%s", out)
	}
}
