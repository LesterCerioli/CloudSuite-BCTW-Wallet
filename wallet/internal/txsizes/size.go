// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package txsizes

import (
	"github.com/btcsuite/btcd/wire"

	h "github.com/btcsuite/btcwallet/internal/helpers"
)

// Worst case script and input/output size estimates.
const (
	// RedeemP2PKHSigScriptSize is the worst case (largest) serialize size
	// of a transaction input script that redeems a compressed P2PKH output.
	// It is calculated as:
	//
	//   - OP_DATA_73
	//   - 72 bytes DER signature + 1 byte sighash
	//   - OP_DATA_33
	//   - 33 bytes serialized compressed pubkey
	RedeemP2PKHSigScriptSize = 1 + 73 + 1 + 33

	// P2PKHPkScriptSize is the size of a transaction output script that
	// pays to a compressed pubkey hash.  It is calculated as:
	//
	//   - OP_DUP
	//   - OP_HASH160
	//   - OP_DATA_20
	//   - 20 bytes pubkey hash
	//   - OP_EQUALVERIFY
	//   - OP_CHECKSIG
	P2PKHPkScriptSize = 1 + 1 + 1 + 20 + 1 + 1

	// RedeemP2PKHInputSize is the worst case (largest) serialize size of a
	// transaction input redeeming a compressed P2PKH output.  It is
	// calculated as:
	//
	//   - 32 bytes previous tx
	//   - 4 bytes output index
	//   - 1 byte compact int encoding value 107
	//   - 107 bytes signature script
	//   - 4 bytes sequence
	RedeemP2PKHInputSize = 32 + 4 + 1 + RedeemP2PKHSigScriptSize + 4

	// P2PKHOutputSize is the serialize size of a transaction output with a
	// P2PKH output script.  It is calculated as:
	//
	//   - 8 bytes output value
	//   - 1 byte compact int encoding value 25
	//   - 25 bytes P2PKH output script
	P2PKHOutputSize = 8 + 1 + P2PKHPkScriptSize

	// RedeemP2WPKHScriptSize is the worst case (largest) serialize size of
	// a transaction input script redeeming a compressed P2WKH output. It
	// is calculated as:
	//
	//   - 72 bytes DER signature + 1 byte sighash
	//   - 33 bytes serialized compressed pubkey
	RedeemP2WPKHScriptSize = 73 + 33

	// P2WKHScriptSize is the size of a transaction output script that pays
	// to a compressed witness pubkey hash.  It is calculated as:
	//
	//	- OP_0: 1 byte
	//	- OP_DATA: 1 byte (PublicKeyHASH160 length)
	//	- PublicKeyHASH160: 20 bytes
	P2WPKHScriptSize = 1 + 1 + 20

	// RedeemP2WKHInputSize is the worst case (largest) serialize size of a
	// transaction input redeeming a compressed P2WKH output.  It is
	// calculated as:
	//
	//   - 32 bytes previous tx
	//   - 4 bytes output index
	//   - 1 byte number of witness elements
	//   - 2 bytes for both witness element lengths
	//   - 106 bytes witness
	//  - 4 bytes sequence
	RedeemP2WKHInputSize = 32 + 4 + 1 + 2 + RedeemP2WPKHScriptSize + 4

	// P2WKHOutputSize is the serialize size of a transaction output with a
	// P2WKH output script.  It is calculated as:
	//
	//   - 8 bytes output value
	//   - 1 byte compact int encoding value 22
	//   - 22 bytes P2PKH output script
	P2WKHOutputSize = 8 + 1 + P2WPKHScriptSize

	// RedeemP2WPKHScriptSize is the worst case (largest) serialize size of
	// a transaction input script redeeming nested p2wkh output. It is
	// calculated as:
	//
	// Script Sig:
	//   - OP_DATA_22: 1 byte
	//   - P2WSH Witness Progam: 22 bytes
	//
	// Witness:
	//   - 72 bytes DER signature + 1 byte sighash
	//   - 33 bytes serialized compressed pubkey
	RedeemNestedP2WPKHScriptSize = 1 + 22 + 73 + 33

	// NestedP2WKHScriptSize is the size of a transaction output script
	// that pays to a nested p2wkh output.  It is calculated as:
	//
	//      - OP_HASH160: 1 byte
	//      - OP_DATA: 1 byte (20 bytes lenght)
	//      - PubKeyHash160: 20 bytes
	//      - OP_EQUAL: 1 byte
	NestedP2WPKHScriptSize = 1 + 1 + 20 + 1

	// RedeemP2WKHInputSize is the worst case (largest) serialize size of a
	// transaction input redeeming a compressed P2WKH output.  It is
	// calculated as:
	//
	//   - 32 bytes previous tx
	//   - 4 bytes output index
	//
	// Witness:
	//   - 1 byte number of witness elements
	//   - 2 bytes for both witness element lengths
	//   - 106 bytes witness
	//
	// Script Sig:
	//   - 1 byte compact int encoding value 107
	//   - 23 bytes signature script
	//
	//   - 4 bytes sequence
	RedeemNestedP2WKHInputSize = 32 + 4 + 1 + 2 + 1 + RedeemNestedP2WPKHScriptSize + 4

	// NestedP2WKHOutputSize is the serialize size of a transaction output
	// with a nested P2WKH output script.  It is calculated as:
	//
	//   - 8 bytes output value
	//   - 1 byte compact int encoding value 23
	//   - 23 bytes P2SH output script
	NestedP2WKHOutputSize = 8 + 1 + NestedP2WPKHScriptSize
)

// EstimateSerializeSize returns a worst case serialize size estimate for a
// signed transaction that spends inputCount number of compressed P2PKH outputs
// and contains each transaction output from txOuts.  The estimated size is
// incremented for an additional P2PKH change output if addChangeOutput is true.
func EstimateSerializeSize(inputCount int, txOuts []*wire.TxOut, addChangeOutput bool) int {
	changeSize := 0
	outputCount := len(txOuts)
	if addChangeOutput {
		changeSize = P2PKHOutputSize
		outputCount++
	}

	// 8 additional bytes are for version and locktime
	return 8 + wire.VarIntSerializeSize(uint64(inputCount)) +
		wire.VarIntSerializeSize(uint64(outputCount)) +
		inputCount*RedeemP2PKHInputSize +
		h.SumOutputSerializeSizes(txOuts) +
		changeSize
}
