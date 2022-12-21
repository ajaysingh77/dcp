package commands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSameIdSamePassword(t *testing.T) {
	var ps passwordSet = make(map[string]passwordData)

	pwd, err := ps.randomPassword("A")
	require.NoError(t, err)

	pwd2, err := ps.randomPassword("A")
	require.NoError(t, err)
	require.Equal(t, pwd, pwd2)

	pwd3, err := ps.randomPassword("B")
	require.NoError(t, err)
	require.NotEqual(t, pwd, pwd3)
}

// Ensures that passwords generated with randomPasswordExt can be referred to via randomPassword
func TestBasicAndExtended(t *testing.T) {
	var ps passwordSet = make(map[string]passwordData)

	pwd, err := ps.randomPasswordExt("A", 8, 8, 3, 2)
	require.NoError(t, err)

	pwd2, err := ps.randomPassword("A")
	require.NoError(t, err)
	require.Equal(t, pwd, pwd2)
}

func TestConfictingComplexityPasswords(t *testing.T) {
	var ps passwordSet = make(map[string]passwordData)

	pwd, err := ps.randomPasswordExt("A", 1, 2, 3, 4)
	require.NoError(t, err)

	_, err = ps.randomPasswordExt("A", 5, 6, 7, 8)
	require.Error(t, err) // Conflicting password complexity requirements

	pwd3, err := ps.randomPasswordExt("A", 1, 2, 3, 4)
	require.NoError(t, err)
	require.Equal(t, pwd, pwd3)

	pwd4, err := ps.randomPassword("A")
	require.NoError(t, err)
	require.Equal(t, pwd, pwd4)
}
