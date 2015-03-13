package consensus

import (
	"errors"
	"math/big"

	"github.com/NebulousLabs/Sia/crypto"
)

var (
	ErrMissingSiacoinOutput = errors.New("transaction spends a nonexisting siacoin output")
	ErrMissingFileContract  = errors.New("transaction terminates a nonexisting file contract")
	ErrMissingSiafundOutput = errors.New("transaction spends a nonexisting siafund output")
)

// FollowsStorageProofRules checks that a transaction follows the limitations
// placed on transactions that have storage proofs.
func (t Transaction) FollowsStorageProofRules() error {
	// No storage proofs, no problems.
	if len(t.StorageProofs) == 0 {
		return nil
	}

	// If there are storage proofs, there can be no siacoin outputs, siafund
	// outputs, or new file contracts.
	if len(t.SiacoinOutputs) != 0 {
		return errors.New("transaction contains storage proofs and siacoin outputs")
	}
	if len(t.FileContracts) != 0 {
		return errors.New("transaction contains storage proofs and file contracts")
	}
	if len(t.FileContractTerminations) != 0 {
		return errors.New("transaction contains storage proofs and file contract terminations")
	}
	if len(t.SiafundOutputs) != 0 {
		return errors.New("transaction contains storage proofs and siafund outputs")
	}

	return nil
}

// SiacoinOutputSum returns the sum of all the siacoin outputs in the
// transaction, which must match the sum of all the siacoin inputs. Siacoin
// outputs created by storage proofs and siafund outputs are not considered, as
// they were considered when the contract responsible for funding them was
// created.
func (t Transaction) SiacoinOutputSum() (sum Currency) {
	// Add the miner fees.
	for _, fee := range t.MinerFees {
		sum = sum.Add(fee)
	}

	// Add the contract payouts
	for _, contract := range t.FileContracts {
		sum = sum.Add(contract.Payout)
	}

	// Add the outputs
	for _, output := range t.SiacoinOutputs {
		sum = sum.Add(output.Value)
	}

	return
}

// validUnlockConditions checks that the unlock conditions have been met
// (signatures are checked elsewhere).
func (s *State) validUnlockConditions(uc UnlockConditions, uh UnlockHash) (err error) {
	if uc.UnlockHash() != uh {
		return errors.New("unlock conditions do not match unlock hash")
	}
	if uc.Timelock > s.height() {
		return errors.New("unlock condition timelock has not been met")
	}

	return
}

// validSiacoins iterates through the inputs of a transaction, summing the
// value of the inputs and checking that the inputs are legal.
func (s *State) validSiacoins(t Transaction) (err error) {
	var inputSum Currency
	for _, sci := range t.SiacoinInputs {
		// Check that the input spends an existing output, and that the
		// UnlockConditions are legal (signatures checked elsewhere).
		sco, exists := s.siacoinOutputs[sci.ParentID]
		if !exists {
			err = ErrMissingSiacoinOutput
			return
		}

		// Check that the unlock conditions are reasonable.
		err = s.validUnlockConditions(sci.UnlockConditions, sco.UnlockHash)
		if err != nil {
			return
		}

		// Add the input value to the sum.
		inputSum = inputSum.Add(sco.Value)
	}
	if inputSum.Cmp(t.SiacoinOutputSum()) != 0 {
		return errors.New("inputs do not equal outputs for transaction")
	}

	return
}

// validFileContracts iterates through the file contracts of a transaction and
// makes sure that each is legal.
func (s *State) validFileContracts(t Transaction) (err error) {
	for _, fc := range t.FileContracts {
		// Check that start and expiration are reasonable values.
		if fc.Start <= s.height() {
			return errors.New("contract must start in the future")
		}
		if fc.Expiration <= fc.Start {
			return errors.New("contract duration must be at least one block")
		}

		// Check that the valid proof outputs sum to the payout after the
		// siafund fee has been applied, and check that the missed proof
		// outputs sum to the full payout.
		var validProofOutputSum, missedProofOutputSum Currency
		for _, output := range fc.ValidProofOutputs {
			validProofOutputSum = validProofOutputSum.Add(output.Value)
		}
		for _, output := range fc.MissedProofOutputs {
			missedProofOutputSum = missedProofOutputSum.Add(output.Value)
		}
		outputPortion := fc.Payout.Sub(fc.Tax())
		if validProofOutputSum.Cmp(outputPortion) != 0 {
			return errors.New("contract valid proof outputs do not sum to the payout minus the siafund fee")
		}
		if missedProofOutputSum.Cmp(fc.Payout) != 0 {
			return errors.New("contract missed proof outputs do not sum to the payout")
		}
	}

	return
}

// validFileContractTerminations checks that each termination in a transaction
// is legal.
func (s *State) validFileContractTerminations(t Transaction) (err error) {
	for _, fct := range t.FileContractTerminations {
		// Check that the FileContractTermination terminates an existing
		// FileContract.
		fc, exists := s.fileContracts[fct.ParentID]
		if !exists {
			return ErrMissingFileContract
		}

		// Check that the unlock conditions are reasonable.
		err = s.validUnlockConditions(fct.TerminationConditions, fc.TerminationHash)
		if err != nil {
			return
		}

		// Check that the payouts in the termination add up to the payout of the
		// contract.
		var payoutSum Currency
		for _, payout := range fct.Payouts {
			payoutSum = payoutSum.Add(payout.Value)
		}
		if payoutSum.Cmp(fc.Payout) != 0 {
			return errors.New("contract termination has incorrect payouts")
		}
	}

	return
}

// storageProofSegment returns the index of the segment that needs to be proven
// exists in a file contract.
func (s *State) storageProofSegment(fcid FileContractID) (index uint64, err error) {
	// Get the file contract associated with the input id.
	fc, exists := s.fileContracts[fcid]
	if !exists {
		err = errors.New("unrecognized file contract id")
		return
	}

	// Get the ID of the trigger block.
	triggerHeight := fc.Start - 1
	if triggerHeight > s.height() {
		err = errors.New("no block found at contract trigger block height")
		return
	}
	triggerID := s.currentPath[triggerHeight]

	// Get the index by appending the file contract ID to the trigger block and
	// taking the hash, then converting the hash to a numerical value and
	// modding it against the number of segments in the file. The result is a
	// random number in range [0, numSegments]. The probability is very
	// slightly weighted towards the beginning of the file, but because the
	// size difference between the number of segments and the random number
	// being modded, the difference is too small to make any practical
	// difference.
	seed := crypto.HashBytes(append(triggerID[:], fcid[:]...))
	numSegments := int64(crypto.CalculateSegments(fc.FileSize))
	seedInt := new(big.Int).SetBytes(seed[:])
	index = seedInt.Mod(seedInt, big.NewInt(numSegments)).Uint64()
	return
}

// validStorageProofs iterates through the storage proofs of a transaction and
// checks that each is legal.
func (s *State) validStorageProofs(t Transaction) error {
	for _, sp := range t.StorageProofs {
		fc, exists := s.fileContracts[sp.ParentID]
		if !exists {
			return errors.New("unrecognized file contract ID in storage proof")
		}

		// Check that the storage proof itself is valid.
		segmentIndex, err := s.storageProofSegment(sp.ParentID)
		if err != nil {
			return err
		}

		verified := crypto.VerifySegment(
			sp.Segment,
			sp.HashSet,
			crypto.CalculateSegments(fc.FileSize),
			segmentIndex,
			fc.FileMerkleRoot,
		)
		if !verified {
			return errors.New("provided storage proof is invalid")
		}
	}

	return nil
}

// validSiafunds checks that the transaction has valid siafund inputs and
// outputs, and that the sum of the inputs matches the sum of the outputs.
func (s *State) validSiafunds(t Transaction) (err error) {
	// Check that all siafund inputs are valid, and get the total number of
	// input siafunds.
	var siafundInputSum Currency
	for _, sfi := range t.SiafundInputs {
		// Check that the siafund output being spent exists.
		sfo, exists := s.siafundOutputs[sfi.ParentID]
		if !exists {
			err = ErrMissingSiafundOutput
			return
		}

		// Check that the unlock conditions are reasonable.
		err = s.validUnlockConditions(sfi.UnlockConditions, sfo.UnlockHash)
		if err != nil {
			return
		}

		// Add this input's value
		siafundInputSum = siafundInputSum.Add(sfo.Value)
	}

	// Check that all siafund outputs are valid and that the siafund output sum
	// is equal to the siafund input sum.
	var siafundOutputSum Currency
	for _, sfo := range t.SiafundOutputs {
		// Check that the claimStart is set to 0. Type safety should enforce
		// this, but check anyway.
		if sfo.ClaimStart.Cmp(ZeroCurrency) != 0 {
			return errors.New("invalid siafund output presented")
		}

		// Add this output's value.
		siafundOutputSum = siafundOutputSum.Add(sfo.Value)
	}
	if siafundOutputSum.Cmp(siafundInputSum) != 0 {
		return errors.New("siafund inputs do not equal siafund outpus within transaction")
	}

	return
}

// validTransaction checks that all fields are valid within the current
// consensus state. If not an error is returned.
func (s *State) validTransaction(t Transaction) (err error) {
	// Check that the storage proof rules are followed.
	err = t.FollowsStorageProofRules()
	if err != nil {
		return
	}

	// Check that each general component of the transaction is valid, without
	// checking signatures.
	err = s.validSiacoins(t)
	if err != nil {
		return
	}
	err = s.validFileContracts(t)
	if err != nil {
		return
	}
	err = s.validFileContractTerminations(t)
	if err != nil {
		return
	}
	err = s.validStorageProofs(t)
	if err != nil {
		return
	}
	err = s.validSiafunds(t)
	if err != nil {
		return
	}

	// Check all of the signatures for validity.
	err = s.validSignatures(t)
	if err != nil {
		return
	}

	return
}