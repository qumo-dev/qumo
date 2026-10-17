package hls

import (
	"testing"

	"github.com/okdaichi/qumo-ledger/ledger/store"
	"github.com/stretchr/testify/assert"
)

// LEDGER_URI accepts every backend: importing the ledger package registers
// the postgres and s3 schemes, which store alone does not.
func TestStoreOpen_RegistersEveryBackend(t *testing.T) {
	assert.Subset(t, store.Schemes(), []string{"mem", "file", "postgres", "postgresql", "s3"})
}
