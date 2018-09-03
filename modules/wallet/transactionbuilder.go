package wallet

import (
	"bytes"
	"errors"
	"math"
	"sort"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/encoding"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"

	"github.com/coreos/bbolt"
)

var (
	// errBuilderAlreadySigned indicates that the transaction builder has
	// already added at least one successful signature to the transaction,
	// meaning that future calls to Sign will result in an invalid transaction.
	errBuilderAlreadySigned = errors.New("sign has already been called on this transaction builder, multiple calls can cause issues")

	// errDustOutput indicates an output is not spendable because it is dust.
	errDustOutput = errors.New("output is too small")

	// errOutputTimelock indicates an output's timelock is still active.
	errOutputTimelock = errors.New("wallet consensus set height is lower than the output timelock")

	// errSpendHeightTooHigh indicates an output's spend height is greater than
	// the allowed height.
	errSpendHeightTooHigh = errors.New("output spend height exceeds the allowed height")
)

// transactionBuilder allows transactions to be manually constructed, including
// the ability to fund transactions with siacoins and siafunds from the wallet.
type transactionBuilder struct {
	// 'signed' indicates that at least one transaction signature has been
	// added to the wallet, meaning that future calls to 'Sign' will fail.
	parents     []types.Transaction
	signed      bool
	transaction types.Transaction

	newParents            []int
	siacoinInputs         []int
	siafundInputs         []int
	transactionSignatures []int

	wallet *Wallet
}

// addSignatures will sign a transaction using a spendable key, with support
// for multisig spendable keys. Because of the restricted input, the function
// is compatible with both siacoin inputs and siafund inputs.
func addSignatures(txn *types.Transaction, cf types.CoveredFields, uc types.UnlockConditions, parentID crypto.Hash, spendKey spendableKey) (newSigIndices []int) {
	// Try to find the matching secret key for each public key - some public
	// keys may not have a match. Some secret keys may be used multiple times,
	// which is why public keys are used as the outer loop.
	totalSignatures := uint64(0)
	for i, siaPubKey := range uc.PublicKeys {
		// Search for the matching secret key to the public key.
		for j := range spendKey.SecretKeys {
			pubKey := spendKey.SecretKeys[j].PublicKey()
			if !bytes.Equal(siaPubKey.Key, pubKey[:]) {
				continue
			}

			// Found the right secret key, add a signature.
			sig := types.TransactionSignature{
				ParentID:       parentID,
				CoveredFields:  cf,
				PublicKeyIndex: uint64(i),
			}
			newSigIndices = append(newSigIndices, len(txn.TransactionSignatures))
			txn.TransactionSignatures = append(txn.TransactionSignatures, sig)
			sigIndex := len(txn.TransactionSignatures) - 1
			sigHash := txn.SigHash(sigIndex)
			encodedSig := crypto.SignHash(sigHash, spendKey.SecretKeys[j])
			txn.TransactionSignatures[sigIndex].Signature = encodedSig[:]

			// Count that the signature has been added, and break out of the
			// secret key loop.
			totalSignatures++
			break
		}

		// If there are enough signatures to satisfy the unlock conditions,
		// break out of the outer loop.
		if totalSignatures == uc.SignaturesRequired {
			break
		}
	}
	return newSigIndices
}

// checkOutput is a helper function used to determine if an output is usable.
func (w *Wallet) checkOutput(tx *bolt.Tx, currentHeight types.BlockHeight, id types.SiacoinOutputID, output types.SiacoinOutput, dustThreshold types.Currency) error {
	// Check that an output is not dust
	if output.Value.Cmp(dustThreshold) < 0 {
		return errDustOutput
	}
	// Check that this output has not recently been spent by the wallet.
	spendHeight, err := dbGetSpentOutput(tx, types.OutputID(id))
	if err == nil {
		if spendHeight+RespendTimeout > currentHeight {
			return errSpendHeightTooHigh
		}
	}
	outputUnlockConditions := w.keys[output.UnlockHash].UnlockConditions
	if currentHeight < outputUnlockConditions.Timelock {
		return errOutputTimelock
	}

	return nil
}

func calculateAmountFromOutputs(outputs []types.SiacoinOutput, fee types.Currency) types.Currency {
	// Calculate the total amount we need to send
	var amount types.Currency
	for i := range outputs {
		output := outputs[i]
		amount = amount.Add(output.Value)
	}

	if fee.Cmp64(0) > 0 {
		amount = amount.Add(fee)
	}
	return amount
}

// FundSiacoinsForOutputs will add enough inputs to cover the outputs to be
// sent in the transaction. In contrast to FundSiacoins, FundSiacoinsForOutputs
// does not aggregate inputs into one output equaling 'amount' - with a refund,
// potentially - for later use by an output or other transaction fee. Rather,
// it aggregates enough inputs to cover the outputs, adds the inputs and outputs
// to the transaction, and also generates a refund output if necessary. A miner
// fee of 0 or greater is also taken into account in the input aggregation and
// added to the transaction if necessary.
func (tb *transactionBuilder) FundSiacoinsForOutputs(outputs []types.SiacoinOutput, fee types.Currency) error {
	// dustThreshold has to be obtained separate from the lock
	dustThreshold, err := tb.wallet.DustThreshold()
	if err != nil {
		return err
	}

	tb.wallet.mu.Lock()
	defer tb.wallet.mu.Unlock()

	consensusHeight, err := dbGetConsensusHeight(tb.wallet.dbTx)
	if err != nil {
		return err
	}

	amount := calculateAmountFromOutputs(outputs, fee)

	// Add a miner fee if the passed fee was greater than 0. The fee also
	// needs to be added to the input amount we need to aggregate.
	if fee.Cmp64(0) > 0 {
		tb.transaction.MinerFees = append(tb.transaction.MinerFees, fee)
	}

	so, err := tb.wallet.getSortedOutputs()
	if err != nil {
		return err
	}

	var fund types.Currency
	// potentialFund tracks the balance of the wallet including outputs that
	// have been spent in other unconfirmed transactions recently. This is to
	// provide the user with a more useful error message in the event that they
	// are overspending.
	var potentialFund types.Currency
	var spentScoids []types.SiacoinOutputID
	for i := range so.ids {
		scoid := so.ids[i]
		sco := so.outputs[i]
		// Check that the output can be spent.
		if err := tb.wallet.checkOutput(tb.wallet.dbTx, consensusHeight, scoid, sco, dustThreshold); err != nil {
			if err == errSpendHeightTooHigh {
				potentialFund = potentialFund.Add(sco.Value)
			}
			continue
		}

		// Add a siacoin input for this output.
		sci := types.SiacoinInput{
			ParentID:         scoid,
			UnlockConditions: tb.wallet.keys[sco.UnlockHash].UnlockConditions,
		}
		tb.siacoinInputs = append(tb.siacoinInputs, len(tb.transaction.SiacoinInputs))
		tb.transaction.SiacoinInputs = append(tb.transaction.SiacoinInputs, sci)
		spentScoids = append(spentScoids, scoid)

		// Add the output to the total fund
		fund = fund.Add(sco.Value)
		potentialFund = potentialFund.Add(sco.Value)
		if fund.Cmp(amount) >= 0 {
			break
		}
	}
	if potentialFund.Cmp(amount) >= 0 && fund.Cmp(amount) < 0 {
		return modules.ErrIncompleteTransactions
	}
	if fund.Cmp(amount) < 0 {
		return modules.ErrLowBalance
	}

	// Add the outputs to the transaction
	for i := range outputs {
		output := outputs[i]
		tb.transaction.SiacoinOutputs = append(tb.transaction.SiacoinOutputs, output)
	}

	// Create a refund output if needed.
	if !amount.Equals(fund) {
		refundUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
		if err != nil {
			return err
		}
		refundOutput := types.SiacoinOutput{
			Value:      fund.Sub(amount),
			UnlockHash: refundUnlockConditions.UnlockHash(),
		}
		tb.transaction.SiacoinOutputs = append(tb.transaction.SiacoinOutputs, refundOutput)
	}

	// Mark all outputs that were spent as spent.
	for _, scoid := range spentScoids {
		err = dbPutSpentOutput(tb.wallet.dbTx, types.OutputID(scoid), consensusHeight)
		if err != nil {
			return err
		}
	}
	return nil
}

// FundSiacoins will add a siacoin input of exactly 'amount' to the
// transaction. A parent transaction may be needed to achieve an input with the
// correct value. The siacoin input will not be signed until 'Sign' is called
// on the transaction builder.
func (tb *transactionBuilder) FundSiacoins(amount types.Currency) error {
	// dustThreshold has to be obtained separate from the lock
	dustThreshold, err := tb.wallet.DustThreshold()
	if err != nil {
		return err
	}

	tb.wallet.mu.Lock()
	defer tb.wallet.mu.Unlock()

	consensusHeight, err := dbGetConsensusHeight(tb.wallet.dbTx)
	if err != nil {
		return err
	}

	// Collect a value-sorted set of siacoin outputs.
	var so sortedOutputs
	err = dbForEachSiacoinOutput(tb.wallet.dbTx, func(scoid types.SiacoinOutputID, sco types.SiacoinOutput) {
		so.ids = append(so.ids, scoid)
		so.outputs = append(so.outputs, sco)
	})
	if err != nil {
		return err
	}
	// Add all of the unconfirmed outputs as well.
	for _, upt := range tb.wallet.unconfirmedProcessedTransactions {
		for i, sco := range upt.Transaction.SiacoinOutputs {
			// Determine if the output belongs to the wallet.
			_, exists := tb.wallet.keys[sco.UnlockHash]
			if !exists {
				continue
			}
			so.ids = append(so.ids, upt.Transaction.SiacoinOutputID(uint64(i)))
			so.outputs = append(so.outputs, sco)
		}
	}
	sort.Sort(sort.Reverse(so))

	// Create and fund a parent transaction that will add the correct amount of
	// siacoins to the transaction.
	var fund types.Currency
	// potentialFund tracks the balance of the wallet including outputs that
	// have been spent in other unconfirmed transactions recently. This is to
	// provide the user with a more useful error message in the event that they
	// are overspending.
	var potentialFund types.Currency
	parentTxn := types.Transaction{}
	var spentScoids []types.SiacoinOutputID
	for i := range so.ids {
		scoid := so.ids[i]
		sco := so.outputs[i]
		// Check that the output can be spent.
		if err := tb.wallet.checkOutput(tb.wallet.dbTx, consensusHeight, scoid, sco, dustThreshold); err != nil {
			if err == errSpendHeightTooHigh {
				potentialFund = potentialFund.Add(sco.Value)
			}
			continue
		}

		// Add a siacoin input for this output.
		sci := types.SiacoinInput{
			ParentID:         scoid,
			UnlockConditions: tb.wallet.keys[sco.UnlockHash].UnlockConditions,
		}
		parentTxn.SiacoinInputs = append(parentTxn.SiacoinInputs, sci)
		spentScoids = append(spentScoids, scoid)

		// Add the output to the total fund
		fund = fund.Add(sco.Value)
		potentialFund = potentialFund.Add(sco.Value)
		if fund.Cmp(amount) >= 0 {
			break
		}
	}
	if potentialFund.Cmp(amount) >= 0 && fund.Cmp(amount) < 0 {
		return modules.ErrIncompleteTransactions
	}
	if fund.Cmp(amount) < 0 {
		return modules.ErrLowBalance
	}

	// Create and add the output that will be used to fund the standard
	// transaction.
	parentUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
	if err != nil {
		return err
	}

	exactOutput := types.SiacoinOutput{
		Value:      amount,
		UnlockHash: parentUnlockConditions.UnlockHash(),
	}
	parentTxn.SiacoinOutputs = append(parentTxn.SiacoinOutputs, exactOutput)

	// Create a refund output if needed.
	if !amount.Equals(fund) {
		refundUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
		if err != nil {
			return err
		}
		refundOutput := types.SiacoinOutput{
			Value:      fund.Sub(amount),
			UnlockHash: refundUnlockConditions.UnlockHash(),
		}
		parentTxn.SiacoinOutputs = append(parentTxn.SiacoinOutputs, refundOutput)
	}

	// Sign all of the inputs to the parent transaction.
	for _, sci := range parentTxn.SiacoinInputs {
		addSignatures(&parentTxn, types.FullCoveredFields, sci.UnlockConditions, crypto.Hash(sci.ParentID), tb.wallet.keys[sci.UnlockConditions.UnlockHash()])
	}
	// Mark the parent output as spent. Must be done after the transaction is
	// finished because otherwise the txid and output id will change.
	err = dbPutSpentOutput(tb.wallet.dbTx, types.OutputID(parentTxn.SiacoinOutputID(0)), consensusHeight)
	if err != nil {
		return err
	}

	// Add the exact output.
	newInput := types.SiacoinInput{
		ParentID:         parentTxn.SiacoinOutputID(0),
		UnlockConditions: parentUnlockConditions,
	}
	tb.newParents = append(tb.newParents, len(tb.parents))
	tb.parents = append(tb.parents, parentTxn)
	tb.siacoinInputs = append(tb.siacoinInputs, len(tb.transaction.SiacoinInputs))
	tb.transaction.SiacoinInputs = append(tb.transaction.SiacoinInputs, newInput)

	// Mark all outputs that were spent as spent.
	for _, scoid := range spentScoids {
		err = dbPutSpentOutput(tb.wallet.dbTx, types.OutputID(scoid), consensusHeight)
		if err != nil {
			return err
		}
	}
	return nil
}

// FundSiafunds will add a siafund input of exactly 'amount' to the
// transaction. A parent transaction may be needed to achieve an input with the
// correct value. The siafund input will not be signed until 'Sign' is called
// on the transaction builder.
func (tb *transactionBuilder) FundSiafunds(amount types.Currency) error {
	tb.wallet.mu.Lock()
	defer tb.wallet.mu.Unlock()

	consensusHeight, err := dbGetConsensusHeight(tb.wallet.dbTx)
	if err != nil {
		return err
	}

	// Create and fund a parent transaction that will add the correct amount of
	// siafunds to the transaction.
	var fund types.Currency
	var potentialFund types.Currency
	parentTxn := types.Transaction{}
	var spentSfoids []types.SiafundOutputID
	c := tb.wallet.dbTx.Bucket(bucketSiafundOutputs).Cursor()
	for idBytes, sfoBytes := c.First(); idBytes != nil; idBytes, sfoBytes = c.Next() {
		var sfoid types.SiafundOutputID
		var sfo types.SiafundOutput
		if err := encoding.Unmarshal(idBytes, &sfoid); err != nil {
			return err
		} else if err := encoding.Unmarshal(sfoBytes, &sfo); err != nil {
			return err
		}

		// Check that this output has not recently been spent by the wallet.
		spendHeight, err := dbGetSpentOutput(tb.wallet.dbTx, types.OutputID(sfoid))
		if err != nil {
			// mimic map behavior: no entry means zero value
			spendHeight = 0
		}
		// Prevent an underflow error.
		allowedHeight := consensusHeight - RespendTimeout
		if consensusHeight < RespendTimeout {
			allowedHeight = 0
		}
		if spendHeight > allowedHeight {
			potentialFund = potentialFund.Add(sfo.Value)
			continue
		}
		outputUnlockConditions := tb.wallet.keys[sfo.UnlockHash].UnlockConditions
		if consensusHeight < outputUnlockConditions.Timelock {
			continue
		}

		// Add a siafund input for this output.
		parentClaimUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
		if err != nil {
			return err
		}
		sfi := types.SiafundInput{
			ParentID:         sfoid,
			UnlockConditions: outputUnlockConditions,
			ClaimUnlockHash:  parentClaimUnlockConditions.UnlockHash(),
		}
		parentTxn.SiafundInputs = append(parentTxn.SiafundInputs, sfi)
		spentSfoids = append(spentSfoids, sfoid)

		// Add the output to the total fund
		fund = fund.Add(sfo.Value)
		potentialFund = potentialFund.Add(sfo.Value)
		if fund.Cmp(amount) >= 0 {
			break
		}
	}
	if potentialFund.Cmp(amount) >= 0 && fund.Cmp(amount) < 0 {
		return modules.ErrIncompleteTransactions
	}
	if fund.Cmp(amount) < 0 {
		return modules.ErrLowBalance
	}

	// Create and add the output that will be used to fund the standard
	// transaction.
	parentUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
	if err != nil {
		return err
	}
	exactOutput := types.SiafundOutput{
		Value:      amount,
		UnlockHash: parentUnlockConditions.UnlockHash(),
	}
	parentTxn.SiafundOutputs = append(parentTxn.SiafundOutputs, exactOutput)

	// Create a refund output if needed.
	if !amount.Equals(fund) {
		refundUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
		if err != nil {
			return err
		}
		refundOutput := types.SiafundOutput{
			Value:      fund.Sub(amount),
			UnlockHash: refundUnlockConditions.UnlockHash(),
		}
		parentTxn.SiafundOutputs = append(parentTxn.SiafundOutputs, refundOutput)
	}

	// Sign all of the inputs to the parent transaction.
	for _, sfi := range parentTxn.SiafundInputs {
		addSignatures(&parentTxn, types.FullCoveredFields, sfi.UnlockConditions, crypto.Hash(sfi.ParentID), tb.wallet.keys[sfi.UnlockConditions.UnlockHash()])
	}

	// Add the exact output.
	claimUnlockConditions, err := tb.wallet.nextPrimarySeedAddress(tb.wallet.dbTx)
	if err != nil {
		return err
	}
	newInput := types.SiafundInput{
		ParentID:         parentTxn.SiafundOutputID(0),
		UnlockConditions: parentUnlockConditions,
		ClaimUnlockHash:  claimUnlockConditions.UnlockHash(),
	}
	tb.newParents = append(tb.newParents, len(tb.parents))
	tb.parents = append(tb.parents, parentTxn)
	tb.siafundInputs = append(tb.siafundInputs, len(tb.transaction.SiafundInputs))
	tb.transaction.SiafundInputs = append(tb.transaction.SiafundInputs, newInput)

	// Mark all outputs that were spent as spent.
	for _, sfoid := range spentSfoids {
		err = dbPutSpentOutput(tb.wallet.dbTx, types.OutputID(sfoid), consensusHeight)
		if err != nil {
			return err
		}
	}
	return nil
}

// UnconfirmedParents returns the unconfirmed parents of the transaction set
// that is being constructed by the transaction builder.
func (tb *transactionBuilder) UnconfirmedParents() (parents []types.Transaction, err error) {
	// Currently we don't need to call UnconfirmedParents after the transaction
	// was signed so we don't allow doing that. If for some reason our
	// requirements change, we can remove this check. The only downside is,
	// that it might lead to transactions being returned that are not actually
	// parents in case the signed transaction already has child transactions.
	if tb.signed {
		return nil, errBuilderAlreadySigned
	}
	addedParents := make(map[types.TransactionID]struct{})
	for _, p := range tb.parents {
		for _, sci := range p.SiacoinInputs {
			tSet := tb.wallet.tpool.TransactionSet(crypto.Hash(sci.ParentID))
			for _, txn := range tSet {
				// Add the transaction to the parents.
				txnID := txn.ID()
				if _, exists := addedParents[txnID]; exists {
					continue
				}
				addedParents[txnID] = struct{}{}
				parents = append(parents, txn)

				// When we found the transaction that contains the output that
				// is spent by sci we stop to avoid adding child transactions.
				for i := range txn.SiacoinOutputs {
					if txn.SiacoinOutputID(uint64(i)) == sci.ParentID {
						break
					}
				}
			}
		}
	}
	return
}

// AddParents adds a set of parents to the transaction.
func (tb *transactionBuilder) AddParents(newParents []types.Transaction) {
	tb.parents = append(tb.parents, newParents...)
}

// AddMinerFee adds a miner fee to the transaction, returning the index of the
// miner fee within the transaction.
func (tb *transactionBuilder) AddMinerFee(fee types.Currency) uint64 {
	tb.transaction.MinerFees = append(tb.transaction.MinerFees, fee)
	return uint64(len(tb.transaction.MinerFees) - 1)
}

// AddSiacoinInput adds a siacoin input to the transaction, returning the index
// of the siacoin input within the transaction. When 'Sign' gets called, this
// input will be left unsigned.
func (tb *transactionBuilder) AddSiacoinInput(input types.SiacoinInput) uint64 {
	tb.transaction.SiacoinInputs = append(tb.transaction.SiacoinInputs, input)
	return uint64(len(tb.transaction.SiacoinInputs) - 1)
}

// AddSiacoinOutput adds a siacoin output to the transaction, returning the
// index of the siacoin output within the transaction.
func (tb *transactionBuilder) AddSiacoinOutput(output types.SiacoinOutput) uint64 {
	tb.transaction.SiacoinOutputs = append(tb.transaction.SiacoinOutputs, output)
	return uint64(len(tb.transaction.SiacoinOutputs) - 1)
}

// AddFileContract adds a file contract to the transaction, returning the index
// of the file contract within the transaction.
func (tb *transactionBuilder) AddFileContract(fc types.FileContract) uint64 {
	tb.transaction.FileContracts = append(tb.transaction.FileContracts, fc)
	return uint64(len(tb.transaction.FileContracts) - 1)
}

// AddFileContractRevision adds a file contract revision to the transaction,
// returning the index of the file contract revision within the transaction.
// When 'Sign' gets called, this revision will be left unsigned.
func (tb *transactionBuilder) AddFileContractRevision(fcr types.FileContractRevision) uint64 {
	tb.transaction.FileContractRevisions = append(tb.transaction.FileContractRevisions, fcr)
	return uint64(len(tb.transaction.FileContractRevisions) - 1)
}

// AddStorageProof adds a storage proof to the transaction, returning the index
// of the storage proof within the transaction.
func (tb *transactionBuilder) AddStorageProof(sp types.StorageProof) uint64 {
	tb.transaction.StorageProofs = append(tb.transaction.StorageProofs, sp)
	return uint64(len(tb.transaction.StorageProofs) - 1)
}

// AddSiafundInput adds a siafund input to the transaction, returning the index
// of the siafund input within the transaction. When 'Sign' is called, this
// input will be left unsigned.
func (tb *transactionBuilder) AddSiafundInput(input types.SiafundInput) uint64 {
	tb.transaction.SiafundInputs = append(tb.transaction.SiafundInputs, input)
	return uint64(len(tb.transaction.SiafundInputs) - 1)
}

// AddSiafundOutput adds a siafund output to the transaction, returning the
// index of the siafund output within the transaction.
func (tb *transactionBuilder) AddSiafundOutput(output types.SiafundOutput) uint64 {
	tb.transaction.SiafundOutputs = append(tb.transaction.SiafundOutputs, output)
	return uint64(len(tb.transaction.SiafundOutputs) - 1)
}

// AddArbitraryData adds arbitrary data to the transaction, returning the index
// of the data within the transaction.
func (tb *transactionBuilder) AddArbitraryData(arb []byte) uint64 {
	tb.transaction.ArbitraryData = append(tb.transaction.ArbitraryData, arb)
	return uint64(len(tb.transaction.ArbitraryData) - 1)
}

// AddTransactionSignature adds a transaction signature to the transaction,
// returning the index of the signature within the transaction. The signature
// should already be valid, and shouldn't sign any of the inputs that were
// added by calling 'FundSiacoins' or 'FundSiafunds'.
func (tb *transactionBuilder) AddTransactionSignature(sig types.TransactionSignature) uint64 {
	tb.transaction.TransactionSignatures = append(tb.transaction.TransactionSignatures, sig)
	return uint64(len(tb.transaction.TransactionSignatures) - 1)
}

// Drop discards all of the outputs in a transaction, returning them to the
// pool so that other transactions may use them. 'Drop' should only be called
// if a transaction is both unsigned and will not be used any further.
func (tb *transactionBuilder) Drop() {
	tb.wallet.mu.Lock()
	defer tb.wallet.mu.Unlock()

	// Iterate through all parents and the transaction itself and restore all
	// outputs to the list of available outputs.
	txns := append(tb.parents, tb.transaction)
	for _, txn := range txns {
		for _, sci := range txn.SiacoinInputs {
			dbDeleteSpentOutput(tb.wallet.dbTx, types.OutputID(sci.ParentID))
		}
	}

	tb.parents = nil
	tb.signed = false
	tb.transaction = types.Transaction{}

	tb.newParents = nil
	tb.siacoinInputs = nil
	tb.siafundInputs = nil
	tb.transactionSignatures = nil
}

// Sign will sign any inputs added by 'FundSiacoins' or 'FundSiafunds' and
// return a transaction set that contains all parents prepended to the
// transaction. If more fields need to be added, a new transaction builder will
// need to be created.
//
// If the whole transaction flag is set to true, then the whole transaction
// flag will be set in the covered fields object. If the whole transaction flag
// is set to false, then the covered fields object will cover all fields that
// have already been added to the transaction, but will also leave room for
// more fields to be added.
//
// Sign should not be called more than once. If, for some reason, there is an
// error while calling Sign, the builder should be dropped.
func (tb *transactionBuilder) Sign(wholeTransaction bool) ([]types.Transaction, error) {
	if tb.signed {
		return nil, errBuilderAlreadySigned
	}

	// Create the coveredfields struct.
	var coveredFields types.CoveredFields
	if wholeTransaction {
		coveredFields = types.CoveredFields{WholeTransaction: true}
	} else {
		for i := range tb.transaction.MinerFees {
			coveredFields.MinerFees = append(coveredFields.MinerFees, uint64(i))
		}
		for i := range tb.transaction.SiacoinInputs {
			coveredFields.SiacoinInputs = append(coveredFields.SiacoinInputs, uint64(i))
		}
		for i := range tb.transaction.SiacoinOutputs {
			coveredFields.SiacoinOutputs = append(coveredFields.SiacoinOutputs, uint64(i))
		}
		for i := range tb.transaction.FileContracts {
			coveredFields.FileContracts = append(coveredFields.FileContracts, uint64(i))
		}
		for i := range tb.transaction.FileContractRevisions {
			coveredFields.FileContractRevisions = append(coveredFields.FileContractRevisions, uint64(i))
		}
		for i := range tb.transaction.StorageProofs {
			coveredFields.StorageProofs = append(coveredFields.StorageProofs, uint64(i))
		}
		for i := range tb.transaction.SiafundInputs {
			coveredFields.SiafundInputs = append(coveredFields.SiafundInputs, uint64(i))
		}
		for i := range tb.transaction.SiafundOutputs {
			coveredFields.SiafundOutputs = append(coveredFields.SiafundOutputs, uint64(i))
		}
		for i := range tb.transaction.ArbitraryData {
			coveredFields.ArbitraryData = append(coveredFields.ArbitraryData, uint64(i))
		}
	}
	// TransactionSignatures don't get covered by the 'WholeTransaction' flag,
	// and must be covered manually.
	for i := range tb.transaction.TransactionSignatures {
		coveredFields.TransactionSignatures = append(coveredFields.TransactionSignatures, uint64(i))
	}

	// For each siacoin input in the transaction that we added, provide a
	// signature.
	tb.wallet.mu.RLock()
	defer tb.wallet.mu.RUnlock()
	for _, inputIndex := range tb.siacoinInputs {
		input := tb.transaction.SiacoinInputs[inputIndex]
		key, ok := tb.wallet.keys[input.UnlockConditions.UnlockHash()]
		if !ok {
			return nil, errors.New("transaction builder added an input that it cannot sign")
		}
		newSigIndices := addSignatures(&tb.transaction, coveredFields, input.UnlockConditions, crypto.Hash(input.ParentID), key)
		tb.transactionSignatures = append(tb.transactionSignatures, newSigIndices...)
		tb.signed = true // Signed is set to true after one successful signature to indicate that future signings can cause issues.
	}
	for _, inputIndex := range tb.siafundInputs {
		input := tb.transaction.SiafundInputs[inputIndex]
		key, ok := tb.wallet.keys[input.UnlockConditions.UnlockHash()]
		if !ok {
			return nil, errors.New("transaction builder added an input that it cannot sign")
		}
		newSigIndices := addSignatures(&tb.transaction, coveredFields, input.UnlockConditions, crypto.Hash(input.ParentID), key)
		tb.transactionSignatures = append(tb.transactionSignatures, newSigIndices...)
		tb.signed = true // Signed is set to true after one successful signature to indicate that future signings can cause issues.
	}

	// Get the transaction set and delete the transaction from the registry.
	txnSet := append(tb.parents, tb.transaction)
	return txnSet, nil
}

// ViewTransaction returns a transaction-in-progress along with all of its
// parents, specified by id. An error is returned if the id is invalid.  Note
// that ids become invalid for a transaction after 'SignTransaction' has been
// called because the transaction gets deleted.
func (tb *transactionBuilder) View() (types.Transaction, []types.Transaction) {
	return tb.transaction, tb.parents
}

// ViewAdded returns all of the siacoin inputs, siafund inputs, and parent
// transactions that have been automatically added by the builder.
func (tb *transactionBuilder) ViewAdded() (newParents, siacoinInputs, siafundInputs, transactionSignatures []int) {
	return tb.newParents, tb.siacoinInputs, tb.siafundInputs, tb.transactionSignatures
}

// registerTransaction takes a transaction and its parents and returns a
// wallet.TransactionBuilder which can be used to expand the transaction. The
// most typical call is 'RegisterTransaction(types.Transaction{}, nil)', which
// registers a new transaction without parents.
func (w *Wallet) registerTransaction(t types.Transaction, parents []types.Transaction) *transactionBuilder {
	// Create a deep copy of the transaction and parents by encoding them. A
	// deep copy ensures that there are no pointer or slice related errors -
	// the builder will be working directly on the transaction, and the
	// transaction may be in use elsewhere (in this case, the host is using the
	// transaction.
	pBytes := encoding.Marshal(parents)
	var pCopy []types.Transaction
	err := encoding.Unmarshal(pBytes, &pCopy)
	if err != nil {
		panic(err)
	}
	tBytes := encoding.Marshal(t)
	var tCopy types.Transaction
	err = encoding.Unmarshal(tBytes, &tCopy)
	if err != nil {
		panic(err)
	}
	return &transactionBuilder{
		parents:     pCopy,
		transaction: tCopy,

		wallet: w,
	}
}

// RegisterTransaction takes a transaction and its parents and returns a
// modules.TransactionBuilder which can be used to expand the transaction. The
// most typical call is 'RegisterTransaction(types.Transaction{}, nil)', which
// registers a new transaction without parents.
func (w *Wallet) RegisterTransaction(t types.Transaction, parents []types.Transaction) (modules.TransactionBuilder, error) {
	if err := w.tg.Add(); err != nil {
		return nil, err
	}
	defer w.tg.Done()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.registerTransaction(t, parents), nil
}

// StartTransaction is a convenience function that calls
// RegisterTransaction(types.Transaction{}, nil).
func (w *Wallet) StartTransaction() (modules.TransactionBuilder, error) {
	if err := w.tg.Add(); err != nil {
		return nil, err
	}
	defer w.tg.Done()
	return w.RegisterTransaction(types.Transaction{}, nil)
}

func (w *Wallet) NewTransaction(outputs []types.SiacoinOutput, fee types.Currency) (tx types.Transaction, err error) {
	tb, err := w.StartTransaction()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			tb.Drop()
		}
	}()
	err = tb.FundSiacoinsForOutputs(outputs, fee)
	if err != nil {
		return
	}
	txnSet, err := tb.Sign(true)
	if err != nil {
		return
	}
	// NOTE: for now, we assume FundSiacoinsForOutputs returns a set with only one tx
	// the transaction builder code is due for an overhaul
	tx = txnSet[0]
	return
}

func (w *Wallet) NewTransactionForAddress(dest types.UnlockHash, amount, fee types.Currency) (tx types.Transaction, err error) {
	output := types.SiacoinOutput{
		Value:      amount,
		UnlockHash: dest,
	}
	return w.NewTransaction([]types.SiacoinOutput{output}, fee)
}

// UnspentOutputs returns the unspent outputs tracked by the wallet.
func (w *Wallet) UnspentOutputs() ([]modules.UnspentOutput, error) {
	if err := w.tg.Add(); err != nil {
		return nil, err
	}
	defer w.tg.Done()
	w.mu.Lock()
	defer w.mu.Unlock()

	// ensure durability of reported outputs
	if err := w.syncDB(); err != nil {
		return nil, err
	}

	// build initial list of confirmed outputs
	var outputs []modules.UnspentOutput
	dbForEachSiacoinOutput(w.dbTx, func(scoid types.SiacoinOutputID, sco types.SiacoinOutput) {
		outputs = append(outputs, modules.UnspentOutput{
			FundType:   types.SpecifierSiacoinOutput,
			ID:         types.OutputID(scoid),
			UnlockHash: sco.UnlockHash,
			Value:      sco.Value,
		})
	})
	dbForEachSiafundOutput(w.dbTx, func(sfoid types.SiafundOutputID, sfo types.SiafundOutput) {
		outputs = append(outputs, modules.UnspentOutput{
			FundType:   types.SpecifierSiafundOutput,
			ID:         types.OutputID(sfoid),
			UnlockHash: sfo.UnlockHash,
			Value:      sfo.Value,
		})
	})

	// don't include outputs marked as spent in pending transactions
	pending := make(map[types.OutputID]struct{})
	for _, pt := range w.unconfirmedProcessedTransactions {
		for _, input := range pt.Inputs {
			if input.WalletAddress {
				pending[input.ParentID] = struct{}{}
			}
		}
	}
	filtered := outputs[:0]
	for _, o := range outputs {
		if _, ok := pending[o.ID]; !ok {
			filtered = append(filtered, o)
		}
	}
	outputs = filtered

	// set the confirmation height for each output
outer:
	for i, o := range outputs {
		txnIndices, err := dbGetAddrTransactions(w.dbTx, o.UnlockHash)
		if err != nil {
			return nil, err
		}
		for _, j := range txnIndices {
			pt, err := dbGetProcessedTransaction(w.dbTx, j)
			if err != nil {
				return nil, err
			}
			for _, sco := range pt.Outputs {
				if sco.ID == o.ID {
					outputs[i].ConfirmationHeight = pt.ConfirmationHeight
					continue outer
				}
			}
		}
	}

	// add unconfirmed outputs, except those that are spent in pending
	// transactions
	for _, pt := range w.unconfirmedProcessedTransactions {
		for _, o := range pt.Outputs {
			if _, ok := pending[o.ID]; !ok && o.WalletAddress {
				outputs = append(outputs, modules.UnspentOutput{
					FundType:           types.SpecifierSiacoinOutput,
					ID:                 o.ID,
					UnlockHash:         o.RelatedAddress,
					Value:              o.Value,
					ConfirmationHeight: types.BlockHeight(math.MaxUint64), // unconfirmed
				})
			}
		}
	}

	return outputs, nil
}

// UnlockConditions returns the UnlockConditions for the specified address, if
// they are known to the wallet.
func (w *Wallet) UnlockConditions(addr types.UnlockHash) (uc types.UnlockConditions, err error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.unlocked {
		return types.UnlockConditions{}, modules.ErrLockedWallet
	}
	if sk, ok := w.keys[addr]; ok {
		uc = sk.UnlockConditions
	} else {
		// not in memory; try database
		uc, err = dbGetUnlockConditions(w.dbTx, addr)
		if err != nil {
			return types.UnlockConditions{}, errors.New("no record of UnlockConditions for that UnlockHash")
		}
	}
	// make a copy of the public key slice; otherwise the caller can modify it
	uc.PublicKeys = append([]types.SiaPublicKey(nil), uc.PublicKeys...)
	return uc, nil
}

// AddUnlockConditions adds a set of UnlockConditions to the wallet database.
func (w *Wallet) AddUnlockConditions(uc types.UnlockConditions) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.unlocked {
		return modules.ErrLockedWallet
	}
	return dbPutUnlockConditions(w.dbTx, uc)
}

// SignTransaction signs txn using secret keys known to the wallet. The
// transaction should be complete with the exception of the Signature fields
// of each TransactionSignature referenced by toSign. For convenience, if
// toSign is empty, SignTransaction signs everything that it can.
func (w *Wallet) SignTransaction(txn *types.Transaction, toSign []crypto.Hash) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.unlocked {
		return modules.ErrLockedWallet
	}
	// if toSign is empty, sign all inputs that we have keys for
	if len(toSign) == 0 {
		for _, sci := range txn.SiacoinInputs {
			if _, ok := w.keys[sci.UnlockConditions.UnlockHash()]; ok {
				toSign = append(toSign, crypto.Hash(sci.ParentID))
			}
		}
		for _, sfi := range txn.SiafundInputs {
			if _, ok := w.keys[sfi.UnlockConditions.UnlockHash()]; ok {
				toSign = append(toSign, crypto.Hash(sfi.ParentID))
			}
		}
	}
	return signTransaction(txn, w.keys, toSign)
}

// SignTransaction signs txn using secret keys derived from seed. The
// transaction should be complete with the exception of the Signature fields
// of each TransactionSignature referenced by toSign, which must not be empty.
//
// SignTransaction must derive all of the keys from scratch, so it is
// appreciably slower than calling the Wallet.SignTransaction method. Only the
// first 1 million keys are derived.
func SignTransaction(txn *types.Transaction, seed modules.Seed, toSign []crypto.Hash) error {
	if len(toSign) == 0 {
		// unlike the wallet method, we can't simply "sign all inputs we have
		// keys for," because without generating all of the keys up front, we
		// don't know how many inputs we actually have keys for.
		return errors.New("toSign cannot be empty")
	}
	// generate keys in batches up to 1e6 before giving up
	keys := make(map[types.UnlockHash]spendableKey, 1e6)
	var keyIndex uint64
	const keysPerBatch = 1000
	for len(keys) < 1e6 {
		for _, sk := range generateKeys(seed, keyIndex, keyIndex+keysPerBatch) {
			keys[sk.UnlockConditions.UnlockHash()] = sk
		}
		keyIndex += keysPerBatch
		if err := signTransaction(txn, keys, toSign); err == nil {
			return nil
		}
	}
	return signTransaction(txn, keys, toSign)
}

// signTransaction signs the specified inputs of txn using the specified keys.
// It returns an error if any of the specified inputs cannot be signed.
func signTransaction(txn *types.Transaction, keys map[types.UnlockHash]spendableKey, toSign []crypto.Hash) error {
	// helper function to lookup unlock conditions in the txn associated with
	// a transaction signature's ParentID
	findUnlockConditions := func(id crypto.Hash) (types.UnlockConditions, bool) {
		for _, sci := range txn.SiacoinInputs {
			if crypto.Hash(sci.ParentID) == id {
				return sci.UnlockConditions, true
			}
		}
		for _, sfi := range txn.SiafundInputs {
			if crypto.Hash(sfi.ParentID) == id {
				return sfi.UnlockConditions, true
			}
		}
		return types.UnlockConditions{}, false
	}
	// helper function to lookup the secret key that can sign
	findSigningKey := func(uc types.UnlockConditions, pubkeyIndex uint64) (crypto.SecretKey, bool) {
		if pubkeyIndex >= uint64(len(uc.PublicKeys)) {
			return crypto.SecretKey{}, false
		}
		pk := uc.PublicKeys[pubkeyIndex]
		sk, ok := keys[uc.UnlockHash()]
		if !ok {
			return crypto.SecretKey{}, false
		}
		for _, key := range sk.SecretKeys {
			pubKey := key.PublicKey()
			if bytes.Equal(pk.Key, pubKey[:]) {
				return key, true
			}
		}
		return crypto.SecretKey{}, false
	}

	for _, id := range toSign {
		// find associated txn signature
		//
		// NOTE: it's possible that the Signature field will already be filled
		// out. Although we could save a bit of work by not signing it, in
		// practice it's probably best to overwrite any existing signatures,
		// since we know that ours will be valid.
		sigIndex := -1
		for i, sig := range txn.TransactionSignatures {
			if sig.ParentID == id {
				sigIndex = i
				break
			}
		}
		if sigIndex == -1 {
			return errors.New("toSign references signatures not present in transaction")
		}
		// find associated input
		uc, ok := findUnlockConditions(id)
		if !ok {
			return errors.New("toSign references IDs not present in transaction")
		}
		// lookup the signing key
		sk, ok := findSigningKey(uc, txn.TransactionSignatures[sigIndex].PublicKeyIndex)
		if !ok {
			return errors.New("could not locate signing key for " + id.String())
		}
		// add signature
		sigHash := txn.SigHash(sigIndex)
		encodedSig := crypto.SignHash(sigHash, sk)
		txn.TransactionSignatures[sigIndex].Signature = encodedSig[:]
	}

	return nil
}
