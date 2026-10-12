package comb

import "testing"

func TestCombAcceptsGameSpecificConsoleAndStop(t *testing.T) {
	result, err := FromMap(map[string]any{"id": "dragonwilds", "console_commands": []string{"listplayers"}, "stop": map[string]any{"command": "quit", "timeout": 90, "signal": "SIGINT"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stop.Command != "quit" || result.Stop.Timeout != 90 || len(result.ConsoleCommands) != 1 {
		t.Fatal("Comb fields were not decoded")
	}
}

func TestCombRejectsInvalidShutdownPolicy(t *testing.T) {
	for _, stop := range []map[string]any{
		{"command": "quit\nother"}, {"timeout": 601}, {"signal": "SIGKILL"},
	} {
		if _, err := FromMap(map[string]any{"id": "test", "stop": stop}); err == nil {
			t.Fatalf("accepted invalid shutdown: %v", stop)
		}
	}
}
