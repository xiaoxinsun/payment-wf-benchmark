package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRepoConfig(t *testing.T) {
	c, err := Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	if c.SLA.Budget != 5*time.Second || c.Timeouts.Step3Validate != 500*time.Millisecond {
		t.Errorf("unexpected values: %+v %+v", c.SLA, c.Timeouts)
	}
	if c.Retries.Step3Validate.MaxAttempts != 4 || c.Retries.Step5Post.MaxAttempts != 0 || c.Retries.Step4bReview.PollEvery != 250*time.Millisecond {
		t.Errorf("unexpected retries: %+v", c.Retries)
	}
	if c.RejectCodes["AC01"] != "PRTRY-AC01" || c.Mix.SimLatency.CBS != 5*time.Millisecond {
		t.Errorf("unexpected maps: %v %+v", c.RejectCodes, c.Mix)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	good, err := Load("../../deploy/config")
	if err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Mix.HappyPath = 50
	if bad.Validate() == nil {
		t.Error("mix not summing to 100 must fail")
	}
	bad = good
	bad.SLA.SettlementShards = 0
	if bad.Validate() == nil {
		t.Error("zero shards must fail")
	}
	bad = good
	bad.Timeouts.Step5Post = 0
	if bad.Validate() == nil {
		t.Error("zero timeout must fail")
	}
	bad = good
	bad.Retries.Step6Respond.Factor = 0.5
	if bad.Validate() == nil {
		t.Error("factor < 1 must fail")
	}
}

func TestLoadMissingOrBrokenFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("missing files must fail")
	}
	dir := t.TempDir()
	for _, f := range []string{"timeouts.yml", "retries.yml", "sla.yml", "mix.yml", "ibps_reject_codes.yml"} {
		os.WriteFile(filepath.Join(dir, f), []byte("::: not yaml [\n"), 0o644)
	}
	if _, err := Load(dir); err == nil {
		t.Error("broken yaml must fail")
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("CBS_URL", "http://x")
	t.Setenv("REPLICAS", "7")
	if EndpointsFromEnv().CBSURL != "http://x" {
		t.Error("env override ignored")
	}
	if EnvInt("REPLICAS", 1) != 7 || EnvInt("NOPE_NOT_SET", 3) != 3 {
		t.Error("EnvInt wrong")
	}
	t.Setenv("REPLICAS", "abc")
	if EnvInt("REPLICAS", 5) != 5 {
		t.Error("bad int must fall back to default")
	}
}
