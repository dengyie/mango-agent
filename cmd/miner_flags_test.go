package cmd

import "testing"

func TestMinerControlCmdFlagIsRegistered(t *testing.T) {
	flag := RootCmd.PersistentFlags().Lookup("miner-control-cmd")
	if flag == nil {
		t.Fatal("missing --miner-control-cmd; mining control would only be configurable via env/JSON")
	}
	api := RootCmd.PersistentFlags().Lookup("miner-api-url")
	if api == nil {
		t.Fatal("missing --miner-api-url")
	}
}
