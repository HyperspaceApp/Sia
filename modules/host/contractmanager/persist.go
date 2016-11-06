package contractmanager

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/NebulousLabs/Sia/build"
	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/persist"
)

type (
	// savedStorageFolder contains fields that are saved automatically to disk
	// for each storage folder.
	savedStorageFolder struct {
		Index uint16
		Path  string
		Usage []uint64
	}

	// savedSettings contains fields that are saved atomically to disk inside
	// of the contract manager directory, alongside the WAL and log.
	savedSettings struct {
		SectorSalt     crypto.Hash
		StorageFolders []savedStorageFolder
	}
)

// savedStorageFolder returns the persistent version of the storage folder.
func (sf *storageFolder) savedStorageFolder() savedStorageFolder {
	return savedStorageFolder{
		Index: sf.index,
		Path:  sf.path,
		Usage: sf.usage,
	}
}

// initSettings will set the default settings for the contract manager.
// initSettings should only be run for brand new contract maangers.
func (cm *ContractManager) initSettings() error {
	// Initialize the sector salt to a random value.
	crypto.Read(cm.sectorSalt[:])

	// Ensure that the initialized defaults have stuck by doing a SaveFileSync
	// with the new settings values.
	ss := cm.savedSettings()
	return build.ExtendErr("error saving contract manager after initialization", persist.SaveFileSync(settingsMetadata, &ss, filepath.Join(cm.persistDir, settingsFile)))
}

// loadSettings will load the contract manager settings.
func (cm *ContractManager) loadSettings() error {
	var ss savedSettings
	err := cm.dependencies.loadFile(settingsMetadata, &ss, filepath.Join(cm.persistDir, settingsFile))
	if os.IsNotExist(err) {
		// There is no settings file, this must be the first time that the
		// contract manager has been run. Initialize with default settings.
		return cm.initSettings()
	} else if err != nil {
		return build.ExtendErr("error loading the contract manager settings file", err)
	}

	// Copy the saved settings into the contract manager.
	cm.sectorSalt = ss.SectorSalt
	for i := range ss.StorageFolders {
		sf := new(storageFolder)
		sf.index = ss.StorageFolders[i].Index
		sf.path = ss.StorageFolders[i].Path
		sf.usage = ss.StorageFolders[i].Usage
		sf.metadataFile, err = cm.dependencies.openFile(filepath.Join(ss.StorageFolders[i].Path, metadataFile), os.O_RDWR, 0700)
		if err != nil {
			return build.ExtendErr("error loading storage folder sector file handle", err)
		}
		sf.sectorFile, err = cm.dependencies.openFile(filepath.Join(ss.StorageFolders[i].Path, sectorFile), os.O_RDWR, 0700)
		if err != nil {
			sf.metadataFile.Close()
			return build.ExtendErr("error loading storage folder sector metadata file handle", err)
		}
		sf.queuedSectors = make(map[sectorID]uint32)
		cm.storageFolders[sf.index] = sf
	}
	return nil
}

// loadSectorLocations will read the metadata portion of each storage folder
// file and load the sector location information into memory.
func (cm *ContractManager) loadSectorLocations() {
	// Each storage folder houses separate sector location data.
	for _, sf := range cm.storageFolders {
		// Read the sector lookup table for this storage folder into memory.
		sectorLookupBytes, err := readFullMetadata(sf.metadataFile, len(sf.usage)*storageFolderGranularity)
		if err != nil {
			cm.log.Printf("Error: unable to read sector metadata for folder %v: %v\n", sf.index, err)
			atomic.AddUint64(&sf.atomicFailedReads, 1)
			continue
		}
		atomic.AddUint64(&sf.atomicSuccessfulReads, 1)

		// Iterate through the sectors that are in-use and read their storage
		// locations into memory.
		for _, sectorIndex := range usageSectors(sf.usage) {
			readHead := sectorMetadataDiskSize * sectorIndex
			var id sectorID
			copy(id[:], sectorLookupBytes[readHead:readHead+12])
			count := binary.LittleEndian.Uint16(sectorLookupBytes[readHead+12 : readHead+14])
			sl := sectorLocation{
				index:         sectorIndex,
				storageFolder: sf.index,
				count:         count,
			}

			// Add the sector to the sector location map.
			cm.sectorLocations[id] = sl
			sf.sectors += 1
		}
	}
}

// savedSettings returns the settings of the contract manager in an
// easily-serializable form.
func (cm *ContractManager) savedSettings() savedSettings {
	ss := savedSettings{
		SectorSalt: cm.sectorSalt,
	}
	for _, sf := range cm.storageFolders {
		// Unset all of the usage bits in the storage folder for the queued sectors.
		for _, sectorIndex := range sf.queuedSectors {
			sf.clearUsage(sectorIndex)
		}

		// Copy over the storage folder.
		ss.StorageFolders = append(ss.StorageFolders, sf.savedStorageFolder())

		// Re-set all of the usage bits for the queued sectors.
		for _, sectorIndex := range sf.queuedSectors {
			sf.setUsage(sectorIndex)
		}
	}
	return ss
}
