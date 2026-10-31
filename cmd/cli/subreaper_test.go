package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandaloneSubreaperMode(t *testing.T) {
	for _, value := range []string{"", "0", "1"} {
		enabled, err := parseStandaloneSubreaperMode(value)
		require.NoError(t, err)
		require.Equal(t, value == "1", enabled)
	}
	for _, value := range []string{"true", "yes", "2", " 1"} {
		_, err := parseStandaloneSubreaperMode(value)
		require.Error(t, err)
	}
}

func TestStandaloneSubreaperPrivateEnvironment(t *testing.T) {
	env := []string{"HOME=/tmp/home", "GO_CLI_LINUX_SUBREAPER=1", "GO_CLI_PRIVATE_REAPER_ROLE=inner", "GO_CLI_PRIVATE_REAPER_NONCE=abc", "GO_CLI_PRIVATE_REAPER_PARENT=7:12", "GO_CLI_PRIVATE_REAPER_EXE=3:4"}
	private, err := readSubreaperPrivateEnvironment(env)
	require.NoError(t, err)
	require.Equal(t, "inner", private["ROLE"])
	require.Equal(t, []string{"HOME=/tmp/home", "GO_CLI_LINUX_SUBREAPER=1"}, stripSubreaperPrivateEnvironment(env))
	_, err = readSubreaperPrivateEnvironment(append(env, "GO_CLI_PRIVATE_REAPER_ROLE=inner"))
	require.Error(t, err, "duplicate role is not an authenticated bootstrap")
	_, err = readSubreaperPrivateEnvironment([]string{"GO_CLI_PRIVATE_REAPER_EXTRA=x"})
	require.Error(t, err, "unknown private key must not bypass startup")
}

func TestStandaloneSubreaperProcessIdentity(t *testing.T) {
	fields := strings.Fields("Z 7 9 9 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 444")
	identity, err := parseSubreaperProcStat("23 (name with ) parens) " + strings.Join(fields, " "))
	require.NoError(t, err)
	require.Equal(t, 23, identity.pid)
	require.Equal(t, 7, identity.parent)
	require.Equal(t, byte('Z'), identity.state)
	require.Equal(t, uint64(444), identity.started)
	_, err = parseSubreaperProcStat("23 (broken) Z 7")
	require.Error(t, err)
}

func TestStandaloneSubreaperPacketRejectsAmbiguity(t *testing.T) {
	valid := []byte(`{"phase":"ready","nonce":"secret","pid":23}`)
	require.NoError(t, validateSubreaperPacket(valid, "ready", "secret", 23))
	for _, data := range []string{
		`{"phase":"ready","phase":"ready","nonce":"secret","pid":23}`,
		`{"phase":"ready","nonce":"wrong","pid":23}`,
		`{"phase":"ready","nonce":"secret","pid":24}`,
		`{"phase":"ready","nonce":"secret","pid":23,"extra":1}`,
		`{"phase":"ready","nonce":"secret","pid":23} {}`,
		`[]`, `null`, `{`,
	} {
		require.Error(t, validateSubreaperPacket([]byte(data), "ready", "secret", 23), "%s", data)
	}
}
