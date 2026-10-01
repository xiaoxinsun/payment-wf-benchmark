package npc

import (
	"math/rand/v2"
	"strconv"
)

func newTestRNG() *rand.Rand { return rand.New(rand.NewPCG(11, 7)) }

func cbsIndex(acct string) int64 {
	n, _ := strconv.ParseInt(acct, 10, 64)
	return n - 6222000000000000
}
