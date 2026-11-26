package postgresfixture_test

import (
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/stretchr/testify/require"
)

func TestInstallIdentity(t *testing.T) {
	p := postgresfixture.Installation{Revision: "v1", Base: []byte("a"), Migrations: []postgresfixture.Migration{{Name: "one", Content: []byte("b")}, {Name: "two", Content: []byte("c")}}}
	n, err := p.TemplateName()
	require.NoError(t, err)
	require.Less(t, len(n), 64)
	first, err := p.Through("one")
	require.NoError(t, err)
	name, err := first.TemplateName()
	require.NoError(t, err)
	require.NotEqual(t, n, name)
	_, err = p.Through("absent")
	require.Error(t, err)
	p.Migrations[1].Content = []byte("d")
	changed, err := p.TemplateName()
	require.NoError(t, err)
	require.NotEqual(t, n, changed)
	p.Migrations[1].Name = "one"
	_, err = p.TemplateName()
	require.Error(t, err)
	_, err = p.Through("one")
	require.Error(t, err)
	_, err = (postgresfixture.Installation{}).TemplateName()
	require.Error(t, err)
	_, err = (postgresfixture.Fixture{}).Clone(t.Context(), p)
	require.Error(t, err)
	require.Error(t, (postgresfixture.Fixture{}).Prepare(t.Context(), p))
	require.Error(t, (postgresfixture.Database{}).Drop(t.Context()))
	_, err = (postgresfixture.Fixture{}).Prune(t.Context(), time.Hour)
	require.Error(t, err)
}
