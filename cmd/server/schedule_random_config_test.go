// schedule_random_config_test.go 排程随机化与积分耗尽冷却的配置层测试。
// 覆盖：缺省（全 0 = 关闭，行为不变）、显式配置、非法值启动期报错、环境变量覆盖。
// [移植自根上游 84e367e / 29b5d0d / c5fc618]
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadJSON(t *testing.T, body string) (*Config, error) {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(fp)
}

// TestScheduleRandomDefaultsOff 缺省全 0：等于没引入这几个键（精确整点 + 800ms 间隔）。
func TestScheduleRandomDefaultsOff(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Schedule.JitterMinutes != 0 || c.Schedule.JitterSalt != "" ||
		c.Schedule.AccountDelayMinSeconds != 0 || c.Schedule.AccountDelayMaxSeconds != 0 ||
		c.Schedule.RandomDelayMinutes != 0 {
		t.Errorf("缺省应全为 0/空，got %+v", c.Schedule)
	}
	if c.CreditExhaustedDur != 0 {
		t.Errorf("credit_exhausted 缺省应关闭，got %v", c.CreditExhaustedDur)
	}
}

// TestScheduleRandomParsed 显式配置生效。
func TestScheduleRandomParsed(t *testing.T) {
	c, err := loadJSON(t, `{"schedule":{"jitter_minutes":15,"jitter_salt":"p1",
		"account_delay_min_seconds":1,"account_delay_max_seconds":4,
		"random_delay_minutes":20},"cooldown":{"credit_exhausted":"10m"}}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Schedule.JitterMinutes != 15 || c.Schedule.JitterSalt != "p1" ||
		c.Schedule.AccountDelayMinSeconds != 1 || c.Schedule.AccountDelayMaxSeconds != 4 ||
		c.Schedule.RandomDelayMinutes != 20 {
		t.Errorf("schedule 未按文件生效: %+v", c.Schedule)
	}
	if c.CreditExhaustedDur != 10*time.Minute {
		t.Errorf("credit_exhausted=%v want 10m", c.CreditExhaustedDur)
	}
}

// TestScheduleRandomValidation 非法值必须启动期报错（静默钳值会让「配了没生效」难查）。
func TestScheduleRandomValidation(t *testing.T) {
	cases := []struct{ name, body, wantSub string }{
		{"jitter 负值", `{"schedule":{"jitter_minutes":-1}}`, "schedule.jitter_minutes"},
		{"jitter 超一天", `{"schedule":{"jitter_minutes":1441}}`, "schedule.jitter_minutes"},
		{"account delay min>max", `{"schedule":{"account_delay_min_seconds":5,"account_delay_max_seconds":2}}`, "account_delay"},
		{"account delay 超上限", `{"schedule":{"account_delay_max_seconds":601}}`, "account_delay"},
		{"account delay 负值", `{"schedule":{"account_delay_min_seconds":-1}}`, "account_delay"},
		{"random_delay 超 60", `{"schedule":{"random_delay_minutes":61}}`, "schedule.random_delay_minutes"},
		{"random_delay 负值", `{"schedule":{"random_delay_minutes":-3}}`, "schedule.random_delay_minutes"},
		{"credit_exhausted 不可解析", `{"cooldown":{"credit_exhausted":"ten"}}`, "cooldown.credit_exhausted"},
		{"credit_exhausted 非正", `{"cooldown":{"credit_exhausted":"0s"}}`, "cooldown.credit_exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadJSON(t, tc.body)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err=%v 应包含 %q", err, tc.wantSub)
			}
		})
	}
}

// TestScheduleRandomBoundaries 合法边界不报错：jitter=1440、delay max=600、random=60。
func TestScheduleRandomBoundaries(t *testing.T) {
	c, err := loadJSON(t, `{"schedule":{"jitter_minutes":1440,"account_delay_min_seconds":0,
		"account_delay_max_seconds":600,"random_delay_minutes":60}}`)
	if err != nil {
		t.Fatalf("边界值不该报错: %v", err)
	}
	if c.Schedule.JitterMinutes != 1440 || c.Schedule.AccountDelayMaxSeconds != 600 ||
		c.Schedule.RandomDelayMinutes != 60 {
		t.Errorf("边界值未生效: %+v", c.Schedule)
	}
}

// TestCreditExhaustedEnvOverride WB2A_CREDIT_EXHAUSTED 覆盖文件值（与既有 WB2A_* 同口径）。
func TestCreditExhaustedEnvOverride(t *testing.T) {
	t.Setenv("WB2A_CREDIT_EXHAUSTED", "15m")
	c, err := loadJSON(t, `{}`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.CreditExhaustedDur != 15*time.Minute {
		t.Errorf("credit_exhausted=%v want 15m（环境变量覆盖）", c.CreditExhaustedDur)
	}
}
