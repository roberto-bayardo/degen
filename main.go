// DEGEN Compoundooooooooooooooooooooooooor (and Dollar Cost Averegooooooooor)!!
//
// WARNING: THIS SCRIPT CAN AND WILL SPEND YOUR WALLET FUNDS AND PROBABLY HAS BUGS. IN THE WORST
// CASE IT MIGHT LOSE ALL YOUR FUNDS. FOR EXPERIMENTATION ONLY. USE WITH ONLY A THROWAWAY WALLET
// THAT HAS LIMITED FUNDS, AND USE AT YOUR OWN RISK!!!
//
// This script will monitor your DEGEN/WETH position on Uniswap v3 and your DEGEN wallet balance,
// and compound any of that uninvested DEGEN back into the pool whenever its value reaches a
// specified threshold.
//
// How to use it: Manually perform at least one transaction to collect fees from your
// position. Find the transaction ID of that transaction, and provide it as input to the script.
// You'll also need to specify either the private key or mnemonic phrase for your wallet so
// transactions can be signed.  E.g.:
//
//    go build main.go
//
//    ./main -txid=your_txid_here --private-key=your_wallet_private_key_here
//
// If you haven't already done so (e.g. when previously adding liquidity to your position), you'll
// also want to grant a large allowance of DEGEN to the uniswap pool because the script does not
// currently grant new allowances.
//
// The script polls the pool at regular intervals, and as soon as the value of your "uninvested
// DEGEN" exceeds a threshold (specified in ETH), it collects any earned fees from the pool, and
// then re-invests any DEGEN that remains in your wallet afterwards. ("uninvested DEGEN" is the
// amount of DEGEN currently in your wallet plus the amount of DEGEN earned by your position that
// remains uncollected.)
//
// Note that you should have extra ETH in the account to cover transaction fees and any slippage.
// The script does not try to ever sell DEGEN to pay for this overhead.
//
// How to make sure it works: lower the investment threshold with the "-threshold" flag to a value
// (in ETH) lower than the value of your uninvested DEGEN, or simply send enough DEGEN to your
// wallet (for example through executing a swap from WETH to DEGEN) to put you over the default
// threshold.
//
// Extra bonus feature: you can also configure the script to dollar cost average ETH into DEGEN at
// a regular interval with the -dca-amount and -dca-interval flags.
//
// TO REITERATE: USE AT YOUR OWN RISK!

package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/base-org/withdrawer/signer"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
)

const (
	defaultL2RPCEndpoint = "https://mainnet.base.org"

	// Address of the WETH/DEGEN .3% fee pool on Base Uniswap v3
	degenPoolAddressString = "0xc9034c3E7F58003E6ae0C8438e7c8f4598d5ACAA"

	// DEGEN token contract
	degenTokenAddressString = "0x4ed4e862860bed51a9570b96d89af5e1b0efefed"

	// Uniswap v3 positions
	positionsAddressString = "0x03a520b32c04bf3beef7beb72e919cf822ed34f1"

	// Contract for executing swaps
	swapAddressString = "0xec8b0f7ffe3ae75d7ffab09429e3675bb63503e4"

	// Default value in ETH of one's uninvested DEGEN that would trigger a compounding
	defaultCompoundingThreshold = .002 // about $5

	// Default minimum number of seconds bewteen executing swaps for dollar cost averaging
	defaultDCAInterval = 3600 * 12 // 12 hours

	// Mininum amount of ETH one must hold in the account for script to proceed, to cover tx fees
	// and such.
	minEthBalance = .001

	// Length of calldata for a call to the Collect() function
	collectCalldataLen = 644

	// Length of calldata for a call to the increaseLiquidity() function
	increaseLiquidityCalldataLen = 452

	// Length of calldata for a call to execute() to swap WETH for DEGEN
	swapCalldataLen = 644

	// How long (in seconds) to sleep before polling wallet & positing stats again
	sleepDuration = 30
)

var (
	degenPoolAddress  common.Address
	degenTokenAddress common.Address
	positionsAddress  common.Address
	swapAddress       common.Address

	//go:embed collect.txt
	collectCalldataHex string
	collectCalldata    []byte

	//go:embed increase-liquidity.txt
	increaseLiquidityCalldataHex string
	increaseLiquidityCalldata    []byte

	//go:embed swap.txt
	swapCalldataHex string
	swapCalldata    []byte

	// Fee cap to use for the on-chain transactions. We set this to 1 gwei, as if it gets higher
	// than this then we probably want to wait for it to come back down. This will keep us from
	// blowing too much eth on tx fees.
	gasFeeCap = big.NewInt(1e9 * params.Wei)
	chainID   = big.NewInt(8453)

	decimals = new(big.Float).SetInt(big.NewInt(1e18))
)

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	degenPoolAddress = common.HexToAddress(degenPoolAddressString)
	degenTokenAddress = common.HexToAddress(degenTokenAddressString)
	positionsAddress = common.HexToAddress(positionsAddressString)
	swapAddress = common.HexToAddress(swapAddressString)

	parseCalldata := func(s string, expectedLen int) []byte {
		calldata, err := hex.DecodeString(strings.TrimSpace(s))
		if err != nil {
			log.Fatalln("Couldn't initialize tx calldata:", err)
		}
		if len(calldata) != expectedLen {
			log.Fatalln("Calldata from embed is wrong length. Expected:", expectedLen, "Got:", len(calldata))
		}
		return calldata
	}

	collectCalldata = parseCalldata(collectCalldataHex, collectCalldataLen)
	increaseLiquidityCalldata = parseCalldata(increaseLiquidityCalldataHex, increaseLiquidityCalldataLen)
	swapCalldata = parseCalldata(swapCalldataHex, swapCalldataLen)
}

// globals
var (
	ctx    context.Context
	client *ethclient.Client

	signWith             signer.Signer  // signer initialized with the user-provided key or mnemonic
	from                 common.Address // the account owner (sender of the bootstrap tx)
	tokenID              []byte         // the tokenID of the user's UniswapV3 position
	compoundingThreshold float64        // value in ETH of one's uninvested DEGEN that would trigger a compounding
	dcaAmount            float64        // amount of ETH to swap with each DCA
	dcaInterval          int64          // minimum amount of time between DCA swaps
	dcaTime              int64          // next (unix) time at which to execute a DCA swap
)

func main() {
	var l2RPCEndpoint string
	var privateKey string
	var mnemonic string
	var hdPath string
	var txid string

	flag.StringVar(&txid, "txid", "", "0x hex ID of a previous fee collection transaction against your position")
	flag.StringVar(&privateKey, "private-key", "", "Private key to use for signing transactions")
	flag.StringVar(&mnemonic, "mnemonic", "", "Mnemonic to use for signing transactions")
	flag.StringVar(&hdPath, "hd-path", "m/44'/60'/0'/0/0", "Hierarchical deterministic derivation path for mnemonic")
	flag.StringVar(&l2RPCEndpoint, "l2-rpc", defaultL2RPCEndpoint, "L2 RPC endpoint url")
	flag.Float64Var(&compoundingThreshold, "threshold", defaultCompoundingThreshold, "threshold (in ETH) on value of uninvested DEGEN to trigger compounding")
	flag.Float64Var(&dcaAmount, "dca-amount", 0.0, "amount of ETH to dollar cost average into the position")
	flag.Int64Var(&dcaInterval, "dca-interval", defaultDCAInterval, "seconds between each dollar cost average swap")
	flag.Parse()

	if txid == "" {
		log.Fatalln("You must specify a -txid of a previous fee collection transaction for your position")
	}
	if privateKey == "" && mnemonic == "" {
		log.Fatalln("One (and only one) of -private-key, -mnemonic must be set")
	}
	dcaTime = time.Now().Unix() + dcaInterval
	var err error
	signWith, err = signer.CreateSigner(privateKey, mnemonic, hdPath)
	if err != nil {
		log.Fatalln("error creating signer:", err)
	}

	ctx = context.Background()
	client, err = ethclient.DialContext(ctx, l2RPCEndpoint)
	if err != nil {
		log.Fatalln("error dialing L2 rpc endpoint:", err)
	}
	defer client.Close()

	tokenID, from, err = tokenAndSenderFromBootstrapTX(txid)
	if err != nil {
		log.Fatalln("failed to process bootstrap tx:", err)
	}
	if signWith.Address() != from {
		log.Fatalln("signer address doesn't match from address from bootstrap tx:", signWith.Address())
	}

	fmt.Printf("\n================= Starting DEGEN/WETH Autocompounding =================\n")
	fmt.Printf("  Account address       : %x\n", from)
	fmt.Printf("  Compounding threshold : %.5g ETH\n", compoundingThreshold)

	for {
		err := loop()
		fmt.Println("\nUnexpected failure:", err)
		fmt.Println("Sleeping for one minute before restarting.")
		time.Sleep(60 * time.Second)
	}
}

func loop() error {
	for {
		ethOwed, degenOwed, degenPerEth, degenFraction, err := getOwed(tokenID)
		if err != nil {
			return fmt.Errorf("getOwed failed: %w", err)
		}
		ethBalance, degenBalance, err := getBalances(tokenID)
		if err != nil {
			return fmt.Errorf("getBalances failed:", err)
		}

		prettyEthBalance := toHuman(ethBalance)
		degenToInvest := new(big.Int).Add(degenOwed, degenBalance)
		degenValue := new(big.Float).SetInt(degenToInvest)
		degenValue.Quo(degenValue, degenPerEth)
		prettyDegenValue := fToHuman(degenValue)

		printStats(prettyEthBalance, toHuman(degenBalance), toHuman(ethOwed), toHuman(degenOwed), prettyDegenValue)

		if prettyEthBalance < minEthBalance {
			log.Fatalf("This account has too little ETH (%f). Make sure you have at least %f ETH to cover tx fees.", prettyEthBalance, minEthBalance)
		}

		if prettyDegenValue >= compoundingThreshold {
			fmt.Printf("\nCompounding threshold met! value=%.5g, threshold=%.5g\n", prettyDegenValue, compoundingThreshold)
			// Don't collect fees if the vast majority of the degen balance is already part of the wallet balance.
			if toHuman(degenOwed) > 100 {
				fmt.Println("\nCollecting owed fees....")
				calldata := getCollectCall(tokenID, ethOwed, degenOwed)
				if err := sendTransaction(positionsAddress, calldata, 300000, new(big.Int)); err != nil {
					return fmt.Errorf("failed to send collect tx: %w", err)
				}
			} else {
				calldata, ethToSend := getIncreaseLiquidityCall(tokenID, degenBalance, degenPerEth, degenFraction)
				if toHuman(ethToSend) > (prettyEthBalance - minEthBalance) {
					return fmt.Errorf("Not enough ETH balance to compound available DEGEN. Please add more ETH to your wallet.")
				}
				fmt.Println("\nCompounding....")
				if err := sendTransaction(positionsAddress, calldata, 500000, ethToSend); err != nil {
					return fmt.Errorf("failed to send increaseLiquidity tx: %w", err)
				}
			}
			time.Sleep(sleepDuration * time.Second)
			continue
		}

		if shouldDCA(prettyEthBalance) {
			fmt.Println("\nDollar cost averaging....")
			calldata, ethToSend := getSwapCall(degenPerEth)
			if err := sendTransaction(swapAddress, calldata, 500000, ethToSend); err != nil {
				return fmt.Errorf("failed to send increaseLiquidity tx: %w", err)
			}
			dcaTime = time.Now().Unix() + dcaInterval
			time.Sleep(sleepDuration * time.Second)
			continue
		}

		time.Sleep(sleepDuration * time.Second)
	} // for
}

func printStats(ethBalance, degenBalance, ethOwed, degenOwed, degenValue float64) {
	fmt.Println()
	fmt.Printf("Uninvested DEGEN value: %.5f ETH\n", degenValue)
	fmt.Printf("  Wallet balances:\n")
	fmt.Printf("    DEGEN %9.f\n", degenBalance)
	fmt.Printf("    ETH   %9.5f\n", ethBalance)
	fmt.Printf("  Unclaimed fees:\n")
	fmt.Printf("    DEGEN %9.f\n", degenOwed)
	fmt.Printf("    ETH   %9.5f\n", ethOwed)
}

func shouldDCA(ethBalance float64) bool {
	if dcaAmount == 0.0 {
		return false
	}
	now := time.Now().Unix()
	if now < dcaTime {
		fmt.Printf("\nTime to next DCA swap: %s\n", time.Duration(dcaTime-now)*time.Second)
		return false
	}
	if dcaAmount > (ethBalance - minEthBalance) {
		fmt.Printf("\nTime to next DCA swap: OVERDUE. Not enough ETH balance to swap.\n")
		return false
	}
	return true
}

func getSwapCall(degenPerEth *big.Float) ([]byte, *big.Int) {
	const (
		deadlineOffset          = 68
		ethToWrapOffset         = 324
		ethToSwapOffset         = 420
		minDegenToReceiveOffset = 452
	)

	calldata := make([]byte, len(swapCalldata))
	copy(calldata, swapCalldata)

	bigF := big.NewFloat(dcaAmount)
	bigF.Mul(bigF, decimals)
	ethToSend, _ := bigF.Int(nil)

	bigF.Mul(bigF, degenPerEth)
	frac := big.NewFloat(.997 * .995) // Adjust for .3% fee & .5% slippage
	bigF.Mul(bigF, frac)
	minDegenToReceive, _ := bigF.Mul(bigF, frac).Int(nil)

	fmt.Println("ETH to swap         :", toHuman(ethToSend))
	fmt.Println("min DEGEN to receive:", toHuman(minDegenToReceive))

	now := time.Now()
	deadline := int(now.Unix()) + 60

	copy(calldata[deadlineOffset:], intToArg(deadline))
	copy(calldata[ethToSwapOffset:], bigToArg(ethToSend))
	copy(calldata[ethToWrapOffset:], bigToArg(ethToSend))
	copy(calldata[minDegenToReceiveOffset:], bigToArg(minDegenToReceive))

	return calldata, ethToSend
}

func getIncreaseLiquidityCall(tokenID []byte, degenBalance *big.Int, degenPerEth *big.Float, degenFraction float64) ([]byte, *big.Int) {
	const (
		tokenIDOffset      = 168
		ethDesiredOffset   = 200
		degenDesiredOffset = 232
		ethMinOffset       = 264
		degenMinOffset     = 296
		deadlineOffset     = 328
	)

	calldata := make([]byte, len(increaseLiquidityCalldata))
	copy(calldata, increaseLiquidityCalldata)

	// Populate the position's tokenID
	copy(calldata[tokenIDOffset:], tokenID)

	// Populate the desired amounts
	f := new(big.Float).SetInt(degenBalance)
	f.Quo(f, big.NewFloat(degenFraction)) // full 1.0 amount
	f.Sub(f, new(big.Float).SetInt(degenBalance))
	f.Quo(f, degenPerEth)
	ethToSend, _ := f.Int(nil)
	fmt.Println("DEGEN to send:", toHuman(degenBalance))
	fmt.Println("ETH to send  :", fToHuman(f))
	copy(calldata[degenDesiredOffset:], bigToArg(degenBalance))
	copy(calldata[ethDesiredOffset:], bigToArg(ethToSend))

	// Populate the min amounts (desired * .005)
	dec := new(big.Int)
	dec.Div(degenBalance, big.NewInt(200)) // .5% of original value
	dec.Sub(degenBalance, dec)
	copy(calldata[degenMinOffset:], bigToArg(dec))
	dec.Div(ethToSend, big.NewInt(200))
	dec.Sub(ethToSend, dec)
	copy(calldata[ethMinOffset:], bigToArg(dec))

	// Populate deadline
	now := time.Now()
	deadline := int(now.Unix()) + 60
	copy(calldata[deadlineOffset:], intToArg(deadline))

	return calldata, ethToSend
}

func getCollectCall(tokenID []byte, ethOwed, degenOwed *big.Int) []byte {
	const (
		tokenIDOffset     = 200
		recipientOffset1  = 424 + 12 // + 12 because we only write last 20 of 32 bytes
		recipientOffset2  = 584 + 12
		ethAmountOffset   = 392
		degenAmountOffset = 552
	)

	calldata := make([]byte, len(collectCalldata))
	copy(calldata, collectCalldata)

	// Populate the position's tokenID
	copy(calldata[tokenIDOffset:], tokenID)

	// Populate the receiver address
	copy(calldata[recipientOffset1:], from[:])
	copy(calldata[recipientOffset2:], from[:])

	// Populate the amounts
	copy(calldata[ethAmountOffset:], bigToArg(ethOwed))
	copy(calldata[degenAmountOffset:], bigToArg(degenOwed))
	return calldata
}

// convert an 18 decimal token amount to human readable float
func toHuman(i *big.Int) float64 {
	fBig := new(big.Float).SetInt(i)
	return fToHuman(fBig)
}

func fToHuman(fBig *big.Float) float64 {
	fBig.Quo(fBig, decimals)
	f, _ := fBig.Float64()
	return f
}

func subIn256(x, y *big.Int) *big.Int {
	diff := new(big.Int).Sub(x, y)
	if diff.Sign() < 0 {
		panic("TODO")
	}
	return diff
}

func selector(sig string) []byte {
	return crypto.Keccak256([]byte(sig))[:4]
}

func slotAsUint(result []byte, slot int) int {
	slotBytes := result[32*slot : 32*slot+32]
	return int(binary.BigEndian.Uint64(slotBytes[24:]))
}

func slotAsBig(result []byte, slot int) *big.Int {
	slotBytes := result[32*slot : 32*slot+32]
	return new(big.Int).SetBytes(slotBytes)
}

func intToArg(arg int) []byte {
	r := make([]byte, 32)
	binary.BigEndian.PutUint64(r[24:], uint64(arg))
	return r
}

func bigToArg(arg *big.Int) []byte {
	r := make([]byte, 32)
	arg.FillBytes(r)
	return r
}

func addressToArg(address common.Address) []byte {
	r := make([]byte, 32)
	copy(r[12:], address[:])
	return r
}

func CallContract(address *common.Address, sel string, args []byte, lenResultExpected int) ([]byte, error) {
	return callContract(address, selector(sel), args, lenResultExpected)
}

func callContract(address *common.Address, selector []byte, args []byte, lenResultExpected int) ([]byte, error) {
	if len(selector) != 4 {
		log.Fatalln("selector should be 4 bytes, got:", len(selector))
	}
	calldata := make([]byte, 4+len(args))
	copy(calldata, selector)
	copy(calldata[4:], args)
	msg := ethereum.CallMsg{
		From: from,
		To:   address,
		Data: calldata,
	}
	result, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, err
	}
	if len(result) != lenResultExpected {
		return nil, fmt.Errorf("contract call result didn't have expected size. got: %d, expected %d", len(result), lenResultExpected)
	}
	return result, nil
}

func tokenAndSenderFromBootstrapTX(txid string) ([]byte, common.Address, error) {
	const (
		tokenIDOffset = 200
	)
	hash := common.HexToHash(txid)
	tx, _, err := client.TransactionByHash(ctx, hash)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("failed to retrieve boostrap tx: %w", err)
	}
	data := tx.Data()
	if len(data) != collectCalldataLen {
		return nil, common.Address{}, fmt.Errorf("transaction doesn't seem to be a collect fee transaction, wrong length:", len(tx.Data()))
	}
	tokenID := data[tokenIDOffset : tokenIDOffset+32]
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("failed to fetch sender from bootstrap tx: %w", err)
	}
	return tokenID, from, nil
}

func getBalances(tokenID []byte) (*big.Int, *big.Int, error) {
	ethBalance, err := client.BalanceAt(ctx, from, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get ETH balance: %w", err)
	}
	degenBalance, err := CallContract(&degenTokenAddress, "balanceOf(address)", addressToArg(from), 32)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get DEGEN balance: %w", err)
	}
	return ethBalance, slotAsBig(degenBalance, 0), nil
}

func getOwed(tokenID []byte) (*big.Int, *big.Int, *big.Float, float64, error) {
	// fetch position info
	result, err := callContract(&positionsAddress, []byte{0x99, 0xfb, 0xab, 0x88}, tokenID, 32*12)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve position info: %w", err)
	}
	lowerTick := slotAsUint(result, 5)
	upperTick := slotAsUint(result, 6)
	liquidity := slotAsBig(result, 7)
	feeGrowthInside0 := slotAsBig(result, 8)
	feeGrowthInside1 := slotAsBig(result, 9)
	tokensOwed0 := slotAsBig(result, 10)
	tokensOwed1 := slotAsBig(result, 11)

	// fetch current swap ratio & tick
	result, err = CallContract(&degenPoolAddress, "slot0()", nil, 224)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve slot0 info: %w", err)
	}
	sqrtPrice := slotAsBig(result, 0)
	currentTick := slotAsUint(result, 1)

	ratio := new(big.Float).SetInt(sqrtPrice)
	q96 := new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)
	ratio.Quo(ratio, new(big.Float).SetInt(q96))
	price, _ := ratio.Float64()
	ratio.Mul(ratio, ratio)
	fRatio, _ := ratio.Float64()

	lowerPrice := math.Sqrt(math.Pow(1.0001, float64(lowerTick)))
	upperPrice := math.Sqrt(math.Pow(1.0001, float64(upperTick)))
	// TODO: handle out of range
	ethPortion := fRatio * (upperPrice - price) / (price * upperPrice)
	degenPortion := (price - lowerPrice)
	total := ethPortion + degenPortion

	degenFraction := degenPortion / total

	if currentTick < upperTick && currentTick >= lowerTick {
		//fmt.Println("Your position is in range!")
	} else {
		return nil, nil, nil, 0.0, fmt.Errorf("can't handle out of range position (yet)")
	}

	liq := toHuman(liquidity)
	degenVal := degenPortion * liq
	totalVal := degenVal / degenFraction
	fmt.Printf("\n-------------------------------------------------\n")
	fmt.Printf("Position stats:\n")
	fmt.Printf("  Total value: %.5f ETH\n", totalVal/fRatio)
	fmt.Printf("  Range: (%d, [current: %d], %d)\n", lowerTick, currentTick, upperTick)
	fmt.Printf("  Portion in DEGEN: %.f%%\n", degenFraction*100.)
	fmt.Printf("  DEGEN in pool:\n")
	fmt.Printf("    Amount: %9.f DEGEN\n", degenVal)
	fmt.Printf("    Value : %9.5f ETH\n", degenVal/fRatio)

	// fetch stats needed to compute collectable fees based on:
	// https://blog.uniswap.org/uniswap-v3-math-primer-2
	result, err = CallContract(&degenPoolAddress, "feeGrowthGlobal0X128()", nil, 32)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve feeGrowthGlobal0: %w", err)
	}
	feeGrowthGlobal0 := slotAsBig(result, 0)

	result, err = CallContract(&degenPoolAddress, "feeGrowthGlobal1X128()", nil, 32)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve feeGrowthGlobal1: %w", err)
	}
	feeGrowthGlobal1 := slotAsBig(result, 0)

	result, err = CallContract(&degenPoolAddress, "ticks(int24)", intToArg(lowerTick), 32*8)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve lower tick info: %w", err)
	}
	lowerFeeGrowthOutside0 := slotAsBig(result, 2)
	lowerFeeGrowthOutside1 := slotAsBig(result, 3)
	result, err = CallContract(&degenPoolAddress, "ticks(int24)", intToArg(upperTick), 32*8)
	if err != nil {
		return nil, nil, nil, 0.0, fmt.Errorf("failed to retrieve upper tick info: %w", err)
	}
	upperFeeGrowthOutside0 := slotAsBig(result, 2)
	upperFeeGrowthOutside1 := slotAsBig(result, 3)

	var upperFeeGrowthAbove0, upperFeeGrowthAbove1 *big.Int
	if currentTick >= upperTick {
		upperFeeGrowthAbove0 = subIn256(feeGrowthGlobal0, upperFeeGrowthOutside0)
		upperFeeGrowthAbove1 = subIn256(feeGrowthGlobal1, upperFeeGrowthOutside1)
	} else {
		upperFeeGrowthAbove0 = upperFeeGrowthOutside0
		upperFeeGrowthAbove1 = upperFeeGrowthOutside1
	}
	var lowerFeeGrowthBelow0, lowerFeeGrowthBelow1 *big.Int
	if currentTick >= lowerTick {
		lowerFeeGrowthBelow0 = lowerFeeGrowthOutside0
		lowerFeeGrowthBelow1 = lowerFeeGrowthOutside1
	} else {
		lowerFeeGrowthBelow0 = subIn256(feeGrowthGlobal0, lowerFeeGrowthOutside0)
		lowerFeeGrowthBelow1 = subIn256(feeGrowthGlobal1, lowerFeeGrowthOutside1)
	}

	fr_t1_0 := subIn256(subIn256(feeGrowthGlobal0, lowerFeeGrowthBelow0), upperFeeGrowthAbove0)
	fr_t1_1 := subIn256(subIn256(feeGrowthGlobal1, lowerFeeGrowthBelow1), upperFeeGrowthAbove1)

	q128 := new(big.Int).Exp(big.NewInt(2), big.NewInt(128), nil)

	uncollectedFees0 := new(big.Int).Mul(liquidity, subIn256(fr_t1_0, feeGrowthInside0))
	uncollectedFees0.Div(uncollectedFees0, q128).Add(uncollectedFees0, tokensOwed0)

	uncollectedFees1 := new(big.Int).Mul(liquidity, subIn256(fr_t1_1, feeGrowthInside1))
	uncollectedFees1.Div(uncollectedFees1, q128).Add(uncollectedFees1, tokensOwed1)

	return uncollectedFees0, uncollectedFees1, ratio, degenFraction, nil
}

func sendTransaction(address common.Address, calldata []byte, gasLimit uint64, value *big.Int) error {
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return fmt.Errorf("failed getting pending nonce: %w", err)
	}

	txMessage := &types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		To:        &address,
		GasTipCap: gasprice.DefaultMinSuggestedPriorityFee,
		GasFeeCap: gasFeeCap,
		Gas:       gasLimit,
		Value:     value,
		Data:      calldata,
	}
	sFn := signWith.SignerFn(chainID)
	collectTX := types.NewTx(txMessage)
	signedCollectTX, err := sFn(from, collectTX)
	if err != nil {
		return fmt.Errorf("failed to sign collect tx: %w", err)
	}

	err = client.SendTransaction(ctx, signedCollectTX)
	if err != nil {
		return fmt.Errorf("failed to collect fees: %w", err)
	}
	fmt.Println("Transaction sent. Awaiting receipt.")

	txid := signedCollectTX.Hash()
	for i := 0; i < 30; i++ { // wait up to a minute before giving up
		r, err := client.TransactionReceipt(ctx, txid)
		if err == ethereum.NotFound {
			time.Sleep(2 * time.Second) // check every block interval for inclusion
			continue
		}
		if err != nil {
			return fmt.Errorf("error waiting for receipt: %w", err)
		}
		fmt.Println("Got receipt. Status:", r.Status)
		if r.Status == 0 {
			return fmt.Errorf("transaction failed (receipt status: 0)")
		}
		return nil
	}
	fmt.Println("Giving up waiting for receipt.")
	return nil
}
