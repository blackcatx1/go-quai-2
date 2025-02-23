package progpow

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/consensus"
	"github.com/dominant-strategies/go-quai/consensus/misc"
	"github.com/dominant-strategies/go-quai/core"
	"github.com/dominant-strategies/go-quai/core/state"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/log"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/dominant-strategies/go-quai/trie"
	"modernc.org/mathutil"
)

// Progpow proof-of-work protocol constants.
var (
	maxUncles                     = 2         // Maximum number of uncles allowed in a single block
	allowedFutureBlockTimeSeconds = int64(15) // Max seconds from current time allowed for blocks, before they're considered future blocks

	ContextTimeFactor = big10
	ZoneBlockReward   = big.NewInt(5e+18)
	RegionBlockReward = new(big.Int).Mul(ZoneBlockReward, big3)
	PrimeBlockReward  = new(big.Int).Mul(RegionBlockReward, big3)
)

// Some useful constants to avoid constant memory allocs for them.
var (
	expDiffPeriod = big.NewInt(100000)
	big0          = big.NewInt(0)
	big1          = big.NewInt(1)
	big2          = big.NewInt(2)
	big3          = big.NewInt(3)
	big8          = big.NewInt(8)
	big9          = big.NewInt(9)
	big10         = big.NewInt(10)
	big32         = big.NewInt(32)
	bigMinus99    = big.NewInt(-99)
	big2e256      = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)) // 2^256
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	errOlderBlockTime      = errors.New("timestamp older than parent")
	errTooManyUncles       = errors.New("too many uncles")
	errDuplicateUncle      = errors.New("duplicate uncle")
	errUncleIsAncestor     = errors.New("uncle is ancestor")
	errDanglingUncle       = errors.New("uncle's parent is not ancestor")
	errInvalidDifficulty   = errors.New("non-positive difficulty")
	errDifficultyCrossover = errors.New("sub's difficulty exceeds dom's")
	errInvalidMixHash      = errors.New("invalid mixHash")
	errInvalidPoW          = errors.New("invalid proof-of-work")
	errInvalidOrder        = errors.New("invalid order")
)

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
func (progpow *Progpow) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase(), nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Quai progpow engine.
func (progpow *Progpow) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	// If we're running a full engine faking, accept any input as valid
	if progpow.config.PowMode == ModeFullFake {
		return nil
	}
	// Short circuit if the header is known, or its parent not
	number := header.NumberU64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash(), number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	return progpow.verifyHeader(chain, header, parent, false, time.Now().Unix())
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
func (progpow *Progpow) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	// If we're running a full engine faking, accept any input as valid
	if progpow.config.PowMode == ModeFullFake || len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}

	// Spawn as many workers as allowed threads
	workers := runtime.GOMAXPROCS(0)
	if len(headers) < workers {
		workers = len(headers)
	}

	// Create a task channel and spawn the verifiers
	var (
		inputs  = make(chan int)
		done    = make(chan int, workers)
		errors  = make([]error, len(headers))
		abort   = make(chan struct{})
		unixNow = time.Now().Unix()
	)
	for i := 0; i < workers; i++ {
		go func() {
			for index := range inputs {
				errors[index] = progpow.verifyHeaderWorker(chain, headers, index, unixNow)
				done <- index
			}
		}()
	}

	errorsOut := make(chan error, len(headers))
	go func() {
		defer close(inputs)
		var (
			in, out = 0, 0
			checked = make([]bool, len(headers))
			inputs  = inputs
		)
		for {
			select {
			case inputs <- in:
				if in++; in == len(headers) {
					// Reached end of headers. Stop sending to workers.
					inputs = nil
				}
			case index := <-done:
				for checked[index] = true; checked[out]; out++ {
					errorsOut <- errors[out]
					if out == len(headers)-1 {
						return
					}
				}
			case <-abort:
				return
			}
		}
	}()
	return abort, errorsOut
}

func (progpow *Progpow) verifyHeaderWorker(chain consensus.ChainHeaderReader, headers []*types.Header, index int, unixNow int64) error {
	var parent *types.Header
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash(), headers[0].NumberU64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash() {
		parent = headers[index-1]
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return progpow.verifyHeader(chain, headers[index], parent, false, unixNow)
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the stock Quai progpow engine.
func (progpow *Progpow) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	// If we're running a full engine faking, accept any input as valid
	if progpow.config.PowMode == ModeFullFake {
		return nil
	}
	// Verify that there are at most 2 uncles included in this block
	if len(block.Uncles()) > maxUncles {
		return errTooManyUncles
	}
	if len(block.Uncles()) == 0 {
		return nil
	}
	// Gather the set of past uncles and ancestors
	uncles, ancestors := mapset.NewSet(), make(map[common.Hash]*types.Header)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestorHeader := chain.GetHeader(parent, number)
		if ancestorHeader == nil {
			break
		}
		ancestors[parent] = ancestorHeader
		// If the ancestor doesn't have any uncles, we don't have to iterate them
		if ancestorHeader.UncleHash() != types.EmptyUncleHash {
			// Need to add those uncles to the banned list too
			ancestor := chain.GetBlock(parent, number)
			if ancestor == nil {
				break
			}
			for _, uncle := range ancestor.Uncles() {
				uncles.Add(uncle.Hash())
			}
		}
		parent, number = ancestorHeader.ParentHash(), number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	// Verify each of the uncles that it's recent, but not an ancestor
	for _, uncle := range block.Uncles() {
		// Make sure every uncle is rewarded only once
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		// Make sure the uncle has a valid ancestry
		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		if ancestors[uncle.ParentHash()] == nil || uncle.ParentHash() == block.ParentHash() {
			return errDanglingUncle
		}
		if err := progpow.verifyHeader(chain, uncle, ancestors[uncle.ParentHash()], true, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules
func (progpow *Progpow) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header, uncle bool, unixNow int64) error {
	nodeCtx := common.NodeLocation.Context()
	// Ensure that the header's extra-data section is of a reasonable size
	if uint64(len(header.Extra())) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra()), params.MaximumExtraDataSize)
	}
	// Verify the header's timestamp
	if !uncle {
		if header.Time() > uint64(unixNow+allowedFutureBlockTimeSeconds) {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time() < parent.Time() {
		return errOlderBlockTime
	}
	// Verify the block's difficulty based on its timestamp and parent's difficulty
	// difficulty adjustment can only be checked in zone
	if nodeCtx == common.ZONE_CTX {
		expected := progpow.CalcDifficulty(chain, parent)
		if expected.Cmp(header.Difficulty()) != 0 {
			return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty(), expected)
		}
	}
	// Verify the engine specific seal securing the block
	_, order, err := progpow.CalcOrder(parent)
	if err != nil {
		return err
	}
	if order > nodeCtx {
		return fmt.Errorf("order of the block is greater than the context")
	}

	if !common.NodeLocation.InSameSliceAs(header.Location()) {
		return fmt.Errorf("block location is not in the same slice as the node location")
	}
	// Verify that the parent entropy is calculated correctly on the header
	parentEntropy := progpow.TotalLogS(parent)
	if parentEntropy.Cmp(header.ParentEntropy()) != 0 {
		return fmt.Errorf("invalid parent entropy: have %v, want %v", header.ParentEntropy(), parentEntropy)
	}
	// If not prime, verify the parentDeltaS field as well
	if nodeCtx > common.PRIME_CTX {
		_, parentOrder, _ := progpow.CalcOrder(parent)
		// If parent was dom, deltaS is zero and otherwise should be the calc delta s on the parent
		if parentOrder < nodeCtx {
			if common.Big0.Cmp(header.ParentDeltaS()) != 0 {
				return fmt.Errorf("invalid parent delta s: have %v, want %v", header.ParentDeltaS(), common.Big0)
			}
		} else {
			parentDeltaS := progpow.DeltaLogS(parent)
			if parentDeltaS.Cmp(header.ParentDeltaS()) != 0 {
				return fmt.Errorf("invalid parent delta s: have %v, want %v", header.ParentDeltaS(), parentDeltaS)
			}
		}
	}
	if nodeCtx == common.ZONE_CTX {
		// check if the header coinbase is in scope
		_, err := header.Coinbase().InternalAddress()
		if err != nil {
			return fmt.Errorf("out-of-scope coinbase in the header")
		}
		// Verify that the gas limit is <= 2^63-1
		cap := uint64(0x7fffffffffffffff)
		if header.GasLimit() > cap {
			return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit(), cap)
		}
		// Verify that the gasUsed is <= gasLimit
		if header.GasUsed() > header.GasLimit() {
			return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed(), header.GasLimit())
		}
		// Verify the block's gas usage and verify the base fee.
		// Verify that the gas limit remains within allowed bounds
		expectedGasLimit := core.CalcGasLimit(parent, progpow.config.GasCeil)
		if expectedGasLimit != header.GasLimit() {
			return fmt.Errorf("invalid gasLimit: have %d, want %d",
				header.GasLimit(), expectedGasLimit)
		}
		// Verify the header is not malformed
		if header.BaseFee() == nil {
			return fmt.Errorf("header is missing baseFee")
		}
		// Verify the baseFee is correct based on the parent header.
		expectedBaseFee := misc.CalcBaseFee(chain.Config(), parent)
		if header.BaseFee().Cmp(expectedBaseFee) != 0 {
			return fmt.Errorf("invalid baseFee: have %s, want %s, parentBaseFee %s, parentGasUsed %d",
				expectedBaseFee, header.BaseFee(), parent.BaseFee(), parent.GasUsed())
		}
	}
	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number(), parent.Number()); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
func (progpow *Progpow) CalcDifficulty(chain consensus.ChainHeaderReader, parent *types.Header) *big.Int {
	nodeCtx := common.NodeLocation.Context()

	if nodeCtx != common.ZONE_CTX {
		log.Error("Cannot CalcDifficulty for", "context", nodeCtx)
		return nil
	}

	///// Algorithm:
	///// e = (DurationLimit - (parent.Time() - parentOfParent.Time())) * parent.Difficulty()
	///// k = Floor(BinaryLog(parent.Difficulty()))/(DurationLimit*DifficultyAdjustmentFactor*AdjustmentPeriod)
	///// Difficulty = Max(parent.Difficulty() + e * k, MinimumDifficulty)

	if parent.Hash() == chain.Config().GenesisHash {
		return parent.Difficulty()
	}
	parentOfParent := chain.GetHeaderByHash(parent.ParentHash())
	if parentOfParent.Hash() == chain.Config().GenesisHash {
		return parent.Difficulty()
	}

	time := parent.Time()
	bigTime := new(big.Int).SetUint64(time)
	bigParentTime := new(big.Int).SetUint64(parentOfParent.Time())

	// holds intermediate values to make the algo easier to read & audit
	x := new(big.Int)
	x.Sub(bigTime, bigParentTime)
	x.Sub(progpow.config.DurationLimit, x)
	x.Mul(x, parent.Difficulty())
	k, _ := mathutil.BinaryLog(new(big.Int).Set(parent.Difficulty()), 64)
	x.Mul(x, big.NewInt(int64(k)))
	x.Div(x, progpow.config.DurationLimit)
	x.Div(x, big.NewInt(params.DifficultyAdjustmentFactor))
	x.Div(x, params.DifficultyAdjustmentPeriod)
	x.Add(x, parent.Difficulty())

	// minimum difficulty can ever be (before exponential factor)
	if x.Cmp(progpow.config.MinDifficulty) < 0 {
		x.Set(progpow.config.MinDifficulty)
	}
	return x
}

func (progpow *Progpow) IsDomCoincident(chain consensus.ChainHeaderReader, header *types.Header) bool {
	_, order, err := progpow.CalcOrder(header)
	if err != nil {
		return false
	}
	return order < common.NodeLocation.Context()
}

func (progpow *Progpow) ComputePowLight(header *types.Header) (mixHash, powHash common.Hash) {
	powLight := func(size uint64, cache []uint32, hash []byte, nonce uint64, blockNumber uint64) ([]byte, []byte) {
		ethashCache := progpow.cache(blockNumber)
		if ethashCache.cDag == nil {
			cDag := make([]uint32, progpowCacheWords)
			generateCDag(cDag, ethashCache.cache, blockNumber/epochLength)
			ethashCache.cDag = cDag
		}
		return progpowLight(size, cache, hash, nonce, blockNumber, ethashCache.cDag)
	}
	cache := progpow.cache(header.NumberU64())
	size := datasetSize(header.NumberU64())
	digest, result := powLight(size, cache.cache, header.SealHash().Bytes(), header.NonceU64(), header.NumberU64(common.ZONE_CTX))
	mixHash = common.BytesToHash(digest)
	powHash = common.BytesToHash(result)
	header.PowDigest.Store(mixHash)
	header.PowHash.Store(powHash)

	// Caches are unmapped in a finalizer. Ensure that the cache stays alive
	// until after the call to hashimotoLight so it's not unmapped while being used.
	runtime.KeepAlive(cache)

	return mixHash, powHash
}

// verifySeal checks whether a block satisfies the PoW difficulty requirements,
// either using the usual progpow cache for it, or alternatively using a full DAG
// to make remote mining fast.
func (progpow *Progpow) verifySeal(header *types.Header) (common.Hash, error) {
	// If we're running a fake PoW, accept any seal as valid
	if progpow.config.PowMode == ModeFake || progpow.config.PowMode == ModeFullFake {
		time.Sleep(progpow.fakeDelay)
		if progpow.fakeFail == header.Number().Uint64() {
			return common.Hash{}, errInvalidPoW
		}
		return common.Hash{}, nil
	}
	// If we're running a shared PoW, delegate verification to it
	if progpow.shared != nil {
		return progpow.shared.verifySeal(header)
	}
	// Ensure that we have a valid difficulty for the block
	if header.Difficulty().Sign() <= 0 {
		return common.Hash{}, errInvalidDifficulty
	}
	// Check progpow
	mixHash := header.PowDigest.Load()
	powHash := header.PowHash.Load()
	if powHash == nil || mixHash == nil {
		mixHash, powHash = progpow.ComputePowLight(header)
	}
	// Verify the calculated values against the ones provided in the header
	if !bytes.Equal(header.MixHash().Bytes(), mixHash.(common.Hash).Bytes()) {
		return common.Hash{}, errInvalidMixHash
	}
	target := new(big.Int).Div(big2e256, header.Difficulty())
	if new(big.Int).SetBytes(powHash.(common.Hash).Bytes()).Cmp(target) > 0 {
		return common.Hash{}, errInvalidPoW
	}
	return powHash.(common.Hash), nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the progpow protocol. The changes are done inline.
func (progpow *Progpow) Prepare(chain consensus.ChainHeaderReader, header *types.Header, parent *types.Header) error {
	header.SetDifficulty(progpow.CalcDifficulty(chain, parent))
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards,
// setting the final state on the header
func (progpow *Progpow) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// Accumulate any block and uncle rewards and commit the final state root
	accumulateRewards(chain.Config(), state, header, uncles)

	if common.NodeLocation.Context() == common.ZONE_CTX && header.ParentHash() == chain.Config().GenesisHash {
		alloc := core.ReadGenesisAlloc("genallocs/gen_alloc_" + common.NodeLocation.Name() + ".json")
		log.Info("Allocating genesis accounts", "num", len(alloc))

		for addressString, account := range alloc {
			addr := common.HexToAddress(addressString)
			internal, err := addr.InternalAddress()
			if err != nil {
				log.Error("Provided address in genesis block is out of scope")
			}
			state.AddBalance(internal, account.Balance)
			state.SetCode(internal, account.Code)
			state.SetNonce(internal, account.Nonce)
			for key, value := range account.Storage {
				state.SetState(internal, key, value)
			}
		}
	}

	header.SetRoot(state.IntermediateRoot(true))
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
func (progpow *Progpow) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, etxs []*types.Transaction, subManifest types.BlockManifest, receipts []*types.Receipt) (*types.Block, error) {
	nodeCtx := common.NodeLocation.Context()
	if nodeCtx == common.ZONE_CTX && chain.ProcessingState() {
		// Finalize block
		progpow.Finalize(chain, header, state, txs, uncles)
	}

	// Header seems complete, assemble into a block and return
	return types.NewBlock(header, txs, uncles, etxs, subManifest, receipts, trie.NewStackTrie(nil)), nil
}

// AccumulateRewards credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included uncles. The coinbase of each uncle block is also rewarded.
func accumulateRewards(config *params.ChainConfig, state *state.StateDB, header *types.Header, uncles []*types.Header) {
	// Select the correct block reward based on chain progression
	blockReward := misc.CalculateReward(header)

	coinbase, err := header.Coinbase().InternalAddress()
	if err != nil {
		log.Error("Block has out-of-scope coinbase, skipping block reward: " + header.Hash().String())
		return
	}

	// Accumulate the rewards for the miner and any included uncles
	reward := new(big.Int).Set(blockReward)
	r := new(big.Int)
	for _, uncle := range uncles {
		coinbase, err := uncle.Coinbase().InternalAddress()
		if err != nil {
			log.Error("Found uncle with out-of-scope coinbase, skipping reward: " + uncle.Hash().String())
			continue
		}
		r.Add(uncle.Number(), big8)
		r.Sub(r, header.Number())
		r.Mul(r, blockReward)
		r.Div(r, big8)
		state.AddBalance(coinbase, r)

		r.Div(blockReward, big32)
		reward.Add(reward, r)
	}
	state.AddBalance(coinbase, reward)
}
