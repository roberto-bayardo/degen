	DEGEN Compoundooooooooooooooooooooooooor (and Dollar Cost Averegooooooooor)!
	
	WARNING: THIS SCRIPT CAN AND WILL SPEND YOUR WALLET FUNDS AND PROBABLY HAS BUGS. IN THE WORST
	CASE IT MIGHT LOSE ALL YOUR FUNDS. FOR EXPERIMENTATION ONLY. USE WITH ONLY A THROWAWAY WALLET
	THAT HAS LIMITED FUNDS, AND USE AT YOUR OWN RISK!!!
	
	This script will monitor your DEGEN/WETH position on Uniswap v3 and your DEGEN wallet balance,
	and compound any of that uninvested DEGEN back into the pool whenever its value reaches a
	specified threshold.
	
	How to use it: Manually perform at least one transaction to collect fees from your
	position. Find the transaction ID of that transaction, and provide it as input to the script.
	You'll also need to specify either the private key or mnemonic phrase for your wallet so
	transactions can be signed.  E.g.:
	
	   go build main.go
	
	   ./main -txid=your_txid_here --private-key=your_wallet_private_key_here
	
	If you haven't already done so (e.g. when previously adding liquidity to your position), you'll
	also want to grant a large allowance of DEGEN to the uniswap pool because the script does not
	currently grant new allowances.
	
	The script polls the pool at regular intervals, and as soon as the value of your "uninvested
	DEGEN" exceeds a threshold (specified in ETH), it collects any earned fees from the pool, and
	then re-invests any DEGEN that remains in your wallet afterwards. ("uninvested DEGEN" is the
	amount of DEGEN currently in your wallet plus the amount of DEGEN earned by your position that
	remains uncollected.)
	
	Note that you should have extra ETH in the account to cover transaction fees and any slippage.
	The script does not try to ever sell DEGEN to pay for this overhead.
	
	How to make sure it works: lower the investment threshold with the "-threshold" flag to a value
	(in ETH) lower than the value of your uninvested DEGEN, or simply send enough DEGEN to your
	wallet (for example through executing a swap from WETH to DEGEN) to put you over the default
	threshold.
	
	Extra bonus feature: you can also configure the script to dollar cost average ETH into DEGEN at
    a regular interval with the -dca-amount and -dca-interval flags.
	
	TO REITERATE: USE AT YOUR OWN RISK!
