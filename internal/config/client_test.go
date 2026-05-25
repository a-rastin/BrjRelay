func TestLoadClient_CoalesceDefaults(t *testing.T) {
    cases := []struct {
        name              string
        json              string // partial JSON: just the coalesce fields
        wantStep, wantMax int
        wantErr           bool
    }{
        {"unset → defaults", `{}`, 25, 50, false},
        {"step zero → defaults", `"coalesce_step_ms": 0`, 25, 50, false},
        {"opt-out", `"coalesce_step_ms": -1`, 0, 0, false},
        {"step only", `"coalesce_step_ms": 40`, 40, 80, false},
        {"step and max", `"coalesce_step_ms": 30, "coalesce_max_ms": 100`, 30, 100, false},
        {"max < step", `"coalesce_step_ms": 50, "coalesce_max_ms": 20`, 0, 0, true},
        {"step too negative", `"coalesce_step_ms": -2`, 0, 0, true},
        {"max negative", `"coalesce_max_ms": -1`, 0, 0, true},
    }
    // ... write a tmpfile per case, call LoadClient, assert
}
