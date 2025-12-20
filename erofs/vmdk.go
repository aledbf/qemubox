// Package erofs provides helpers for EROFS-related image handling.
package erofs

import (
	"fmt"
	"io"
	"os"
)

const (
	max2GbExtentSectors = 0x80000000 >> 9
	sectorsPerTrack     = 63
	numberHeads         = 16
	subformat           = "twoGbMaxExtentFlat"
	adapterType         = "ide"
	hwVersion           = "4"
)

// vmdkDescAddExtent writes extent lines to the writer.
// Each extent line follows the format: RW <count> FLAT "<filename>" <offset>
func vmdkDescAddExtent(w io.Writer, sectors uint64, filename string, offset uint64) error {
	for sectors > 0 {
		count := min(sectors, max2GbExtentSectors)

		_, err := fmt.Fprintf(w, "RW %d FLAT \"%s\" %d\n", count, filename, offset)
		if err != nil {
			return err
		}
		offset += count
		sectors -= count
	}
	return nil
}

// DumpVMDKDescriptor writes a VMDK descriptor to the provided writer.
func DumpVMDKDescriptor(w io.Writer, cid uint32, devices []string) error {
	parentCID := uint32(0xffffffff)

	_, err := fmt.Fprintf(w, `# Disk DescriptorFile
version=1
CID=%08x
parentCID=%08x
createType="%s"

# Extent description
`, cid, parentCID, subformat)
	if err != nil {
		return err
	}

	totalSectors := uint64(0)

	for _, d := range devices {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		sectors := uint64(fi.Size()) >> 9
		err = vmdkDescAddExtent(w, sectors, d, 0)
		if err != nil {
			return err
		}
		totalSectors += sectors
	}

	cylinders := (totalSectors + sectorsPerTrack*numberHeads - 1) / (sectorsPerTrack * numberHeads)
	_, err = fmt.Fprintf(w, `

# The Disk Data Base
#DDB

ddb.virtualHWVersion = "%s"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "%d"
ddb.geometry.sectors = "63"
ddb.adapterType = "%s"
`, hwVersion, cylinders, numberHeads, adapterType)
	if err != nil {
		return err
	}
	return nil
}

// DumpVMDKDescriptorToFile writes a VMDK descriptor to the given file path.
func DumpVMDKDescriptorToFile(vmdkdesc string, cid uint32, devices []string) error {
	f, err := os.Create(vmdkdesc)
	if err != nil {
		return err
	}
	if err := DumpVMDKDescriptor(f, cid, devices); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
