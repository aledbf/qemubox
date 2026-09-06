# The device set QEMU is built with, passed as `--with-devices-x86_64=spin`.
#
# A file and not a `sed`/`echo >>` over the source tree, because that tree lives in a
# BuildKit cache mount keyed by version alone: an edit made there survives every later
# build, including ones meant to undo it, and — worse — it is applied inside the
# `if [ ! -f config.status ]` branch, so whether the tree being compiled is the tree
# upstream shipped depends on whether some earlier build happened to configure. This file
# is copied in and read on every build.
#
# **One device per function, and the function's most compatible device.** For a Linux guest
# that is virtio without qualification: in mainline since 2.6.25 and in every distribution's
# kernel. e1000 and rtl8139 are "more compatible" only for a guest with no virtio drivers,
# which is not what this runs. Every extra model is a device a guest could be given by
# accident, a driver its kernel has to carry, and for a NIC an option ROM QEMU refuses to
# start without.
#
# What the VM actually gets is in internal/host/vm/qemu/qemu_command.go, and it is all
# virtio: virtio-blk-pci, virtio-net-pci, virtio-rng-pci, vhost-vsock-pci and virtconsole,
# on a q35 started with -nodefaults. Everything below follows from that one list.
#
# Only symbols upstream marks optional are touched — the ones carrying
# `default y if PCI_DEVICES` (or ISA/PCIE/USB) in hw/*/Kconfig. Nothing `select`s them, so
# each goes on its own and PCI_DEVICES itself stays. Turning off a symbol upstream does not
# offer produces a link failure twenty minutes in, at `make`, which configure cannot warn
# about; every change here is checked by running scripts/minikconf.py against this file
# before a build is started.
include ../i386-softmmu/default.mak

# --- boards ---------------------------------------------------------------------------
# q35 is the only machine started here (start.go). The rest are a 1996 PC and a machine
# type for AWS enclaves. MICROVM stays: it costs nothing and it is the obvious next
# experiment for boot time.
CONFIG_ISAPC=n
CONFIG_I440FX=n
CONFIG_NITRO_ENCLAVE=n

# --- network: one card ------------------------------------------------------------------
# virtio-net, and it is already started with `romfile=` so not even its option ROM is
# loaded. An emulated e1000 in a microVM is a performance bug with a driver.
CONFIG_E1000_PCI=n
CONFIG_E1000E_PCI_EXPRESS=n
CONFIG_IGB_PCI_EXPRESS=n
CONFIG_EEPRO100_PCI=n
CONFIG_NE2000_PCI=n
CONFIG_NE2000_ISA=n
CONFIG_PCNET_PCI=n
CONFIG_RTL8139_PCI=n
CONFIG_TULIP=n
CONFIG_VMXNET3_PCI=n
CONFIG_ROCKER=n
CONFIG_USB_NETWORK=n
CONFIG_CAN_SJA1000=n
CONFIG_CAN_PCI=n
CONFIG_CAN_CTUCANFD=n
CONFIG_CAN_CTUCANFD_PCI=n

# --- storage: one disk ------------------------------------------------------------------
# virtio-blk, which is what every mount becomes (internal/shim/platform/mounts). The HBAs
# below emulate 1990s hardware. AHCI is not in the list because Q35 selects AHCI_ICH9 —
# the ICH9 southbridge has SATA whether or not anything is plugged into it.
CONFIG_NVME_PCI=n
CONFIG_LSI_SCSI_PCI=n
CONFIG_MPTSAS_SCSI_PCI=n
CONFIG_MEGASAS_SCSI_PCI=n
CONFIG_VMW_PVSCSI_SCSI_PCI=n
CONFIG_ESP_PCI=n
CONFIG_SDHCI_PCI=n
CONFIG_UFS_PCI=n

# --- no display, no sound, no USB, no serial over PCI -------------------------------------
# The console is virtconsole over virtio-serial, and the VM starts with -nodefaults and
# -nographic, so no display adapter is ever created — which is why vgabios-stdvga.bin is no
# longer shipped with the firmware. Audio has no backend compiled in; USB is a bus these
# guests have no devices on.
#
# CONFIG_VGA_PCI stays, and it is the one entry here that a build had to find: upstream
# offers it, minikconf accepts turning it off, and then `make` fails twenty minutes later
# with `undefined reference to pci_std_vga_mmio_region_init` — hw/display/virtio-vga.c
# calls into vga-pci.c, and virtio-vga is not a symbol upstream offers at this level. The
# model stays compiled and unused; nothing creates it.
CONFIG_VGA_CIRRUS=n
CONFIG_VMWARE_VGA=n
CONFIG_BOCHS_DISPLAY=n
CONFIG_ATI_VGA=n
CONFIG_MAC_PVG_PCI=n
CONFIG_ES1370=n
CONFIG_AC97=n
CONFIG_HDA=n
CONFIG_SERIAL_PCI=n
CONFIG_SERIAL_PCI_MULTI=n
CONFIG_USB_UHCI=n
CONFIG_USB_OHCI_PCI=n
CONFIG_USB_EHCI_PCI=n
CONFIG_USB_XHCI_PCI=n
CONFIG_USB_XHCI_NEC=n

# --- legacy PC and the rest ---------------------------------------------------------------
CONFIG_APPLESMC=n
CONFIG_TEST_DEVICES=n
CONFIG_QXL=n
CONFIG_ISA_DEBUG=n
CONFIG_ISA_IPMI_BT=n
CONFIG_ISA_IPMI_KCS=n
CONFIG_PCI_IPMI_BT=n
CONFIG_PCI_IPMI_KCS=n
CONFIG_IPMI_SSIF=n
CONFIG_HYPERV=n
CONFIG_WDT_IB6300ESB=n
CONFIG_TPCI200=n
CONFIG_IVSHMEM_DEVICE=n

# Two that upstream's default.mak offers and 11.1.1 refuses, each found by running
# scripts/minikconf.py rather than by reading the list:
#   CONFIG_SGA=n  — "undefined symbol SGA": the device is gone from the Kconfig tree and
#                   the line survives in upstream's default.mak alone.
#   CONFIG_FDC=n  — "contradiction between clauses when setting FDC": q35's ICH9 brings an
#                   ISA_SUPERIO, which selects FDC_ISA, which selects FDC.
# and a third that only `make` can find, recorded at CONFIG_VGA_PCI above.
#
# And one this build used to set and no longer does: CONFIG_CXL=n. Upstream does not offer
# it, and turning it off broke the 11.0.2 link (ACPI and the PCI expander bridge still
# reference cxl_component_register_block_init). It was appended to the source tree's
# default.mak from inside the "only if not configured yet" branch, so whether it applied at
# all depended on whether the build cache happened to be cold.
#
# Deliberately left on:
#   PCI_DEVICES  — q35 references controllers this would take with it, and every model
#                  above is reachable one at a time without it.
#   PCI_BRIDGE, PCIE_PORT, XIO3130, IOH3420, I82801B11 — a q35's root ports.
#   VIRTIO_PCI, VIRTIO_BLK, VIRTIO_NET, VIRTIO_RNG, VIRTIO_SERIAL, VHOST_VSOCK,
#   VIRTIO_BALLOON, VIRTIO_MEM, DIMM — the devices this VM is actually given, plus the
#   memory hotplug path (internal/shim/memhotplug).
#   VTD, AMD_IOMMU — VFIO on q35 needs them.
#   HPET, PVPANIC — the machine line says hpet=off, which is a property of the device
#                   being there; pvpanic is how a guest reports a panic.
#   SEV, TDX, SGX — confidential computing is a product decision, not debloat.
