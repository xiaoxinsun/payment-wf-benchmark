// Package config loads every timeout, retry, SLA and mix value from deploy/config so that all variants
// run with exactly the same numbers.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Timeouts struct {
	Step3Validate    time.Duration `yaml:"step3_validate"`
	Step4Screen      time.Duration `yaml:"step4_screen"`
	Step4bReviewPoll time.Duration `yaml:"step4b_review_poll"`
	Step5Post        time.Duration `yaml:"step5_post"`
	Step5StatusQuery time.Duration `yaml:"step5_status_query"`
	Step6Respond     time.Duration `yaml:"step6_respond"`
}

// RetryPolicy: MaxAttempts 0 means unbounded. BoundedBySLA policies stop at the SLA deadline.
type RetryPolicy struct {
	Initial      time.Duration `yaml:"initial"`
	Factor       float64       `yaml:"factor"`
	MaxInterval  time.Duration `yaml:"max_interval"`
	MaxAttempts  int           `yaml:"max_attempts"`
	BoundedBySLA bool          `yaml:"bounded_by_sla"`
	PollEvery    time.Duration `yaml:"poll_every"`
}

type Retries struct {
	Step3Validate RetryPolicy `yaml:"step3_validate"`
	Step4Screen   RetryPolicy `yaml:"step4_screen"`
	Step4bReview  RetryPolicy `yaml:"step4b_review"`
	Step5Post     RetryPolicy `yaml:"step5_post"`
	Step6Respond  RetryPolicy `yaml:"step6_respond"`
}

type SLA struct {
	Budget           time.Duration `yaml:"sla_budget"`
	AmountLimitFen   int64         `yaml:"amount_limit_cny_fen"`
	OurBankCode      string        `yaml:"our_bank_code"`
	SettlementShards int           `yaml:"settlement_shards"`
}

type SimLatency struct {
	CBS time.Duration `yaml:"cbs"`
	AML time.Duration `yaml:"aml"`
	NPC time.Duration `yaml:"npc"`
}

type Mix struct {
	HappyPath     int        `yaml:"happy_path"`
	AccountReject int        `yaml:"account_reject"`
	AMLHit        int        `yaml:"aml_hit"`
	ReviewFast    int        `yaml:"review_fast"`
	ReviewSlow    int        `yaml:"review_slow"`
	SimLatency    SimLatency `yaml:"sim_latency"`
}

type Config struct {
	Timeouts    Timeouts
	Retries     Retries
	SLA         SLA
	Mix         Mix
	RejectCodes map[string]string
}

func loadYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Load reads timeouts.yml, retries.yml, sla.yml, mix.yml and ibps_reject_codes.yml from dir.
func Load(dir string) (Config, error) {
	var c Config
	for file, out := range map[string]any{
		"timeouts.yml": &c.Timeouts, "retries.yml": &c.Retries, "sla.yml": &c.SLA,
		"mix.yml": &c.Mix, "ibps_reject_codes.yml": &c.RejectCodes,
	} {
		if err := loadYAML(filepath.Join(dir, file), out); err != nil {
			return Config{}, err
		}
	}
	if n := EnvInt("SETTLEMENT_SHARDS", 0); n > 0 { // S5 runs the same binaries with a different shard count
		c.SLA.SettlementShards = n
	}
	return c, c.Validate()
}

// Validate rejects configs that would silently break the benchmark.
func (c Config) Validate() error {
	if c.SLA.Budget <= 0 || c.SLA.SettlementShards < 1 || c.SLA.AmountLimitFen <= 0 || c.SLA.OurBankCode == "" {
		return fmt.Errorf("invalid sla config: %+v", c.SLA)
	}
	if c.Timeouts.Step3Validate <= 0 || c.Timeouts.Step4Screen <= 0 || c.Timeouts.Step5Post <= 0 || c.Timeouts.Step6Respond <= 0 {
		return fmt.Errorf("invalid timeouts: %+v", c.Timeouts)
	}
	if c.Mix.HappyPath+c.Mix.AccountReject+c.Mix.AMLHit+c.Mix.ReviewFast+c.Mix.ReviewSlow != 100 {
		return fmt.Errorf("mix must sum to 100: %+v", c.Mix)
	}
	for name, p := range map[string]RetryPolicy{"step3": c.Retries.Step3Validate, "step4": c.Retries.Step4Screen, "step5": c.Retries.Step5Post, "step6": c.Retries.Step6Respond} {
		if p.Initial <= 0 || p.Factor < 1 || p.MaxInterval < p.Initial {
			return fmt.Errorf("invalid retry policy %s: %+v", name, p)
		}
	}
	return nil
}

// Endpoints are the addresses of external systems and databases (from the environment).
type Endpoints struct {
	CBSURL, AMLURL, NPCURL string
	AppDBURL               string
	ConfigDir              string
	MetricsAddr            string
	ListenAddr             string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func EndpointsFromEnv() Endpoints {
	return Endpoints{
		CBSURL:      env("CBS_URL", "http://localhost:8001"),
		AMLURL:      env("AML_URL", "http://localhost:8002"),
		NPCURL:      env("NPC_URL", "http://localhost:9000"),
		AppDBURL:    env("APP_DB_URL", "postgres://bench:bench@localhost:5435/app?sslmode=disable&pool_max_conns=20"),
		ConfigDir:   env("CONFIG_DIR", "deploy/config"),
		MetricsAddr: env("METRICS_ADDR", ":9100"),
		ListenAddr:  env("LISTEN_ADDR", ":8080"),
	}
}

// EnvInt reads an integer environment variable with a default.
func EnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
