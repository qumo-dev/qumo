package relay

import (
	"testing"

	"github.com/qumo-dev/qumo/internal/credential"
	"github.com/stretchr/testify/assert"
)

func TestAdmissions_CountsPerProjectAndRole(t *testing.T) {
	var a admissions
	publisher := func(project string) *admission {
		return &admission{cred: credential.Credential{Key: credential.Key{ProjectID: project}}}
	}
	p1a, p1b, p2 := publisher("p1"), publisher("p1"), publisher("p2")
	sub := &admission{subscriber: true, cred: credential.Credential{Key: credential.Key{ProjectID: "p1"}}}

	for _, ad := range []*admission{p1a, p1b, p2, sub} {
		a.add(ad)
	}
	assert.Equal(t, 2, a.count("p1", false))
	assert.Equal(t, 1, a.count("p1", true))
	assert.Equal(t, 1, a.count("p2", false))

	a.remove(p1a)
	a.remove(p1a) // removal is idempotent

	assert.Equal(t, 1, a.count("p1", false))
	assert.Zero(t, a.count("p3", false))
}
