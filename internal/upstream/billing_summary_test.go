package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func fmtMs(v int64) string { return strconv.FormatInt(v, 10) }

// billingData 真实国际版形状：业务字段直接在 data 下（2026-10-23 抓包核对）。
func billingData(inner string) string {
	return `{"code":0,"data":` + inner + `}`
}

// billingDataLegacy CN 风格信封（data.Response.Data）：兼容层仍需能解。
func billingDataLegacy(inner string) string {
	return `{"code":0,"data":{"Response":{"Data":` + inner + `}}}`
}

func TestFlexFloatForms(t *testing.T) {
	var got struct {
		A flexFloat `json:"a"`
		B flexFloat `json:"b"`
		C flexFloat `json:"c"`
		D flexFloat `json:"d"`
	}
	raw := `{"a":"335.47000026","b":200,"c":null,"d":"oops"}`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if float64(got.A) != 335.47000026 || float64(got.B) != 200 || got.C != 0 || got.D != 0 {
		t.Fatalf("flexFloat=%v/%v/%v/%v", got.A, got.B, got.C, got.D)
	}
}

func TestGlobalUserResourceSummaryMerge(t *testing.T) {
	auth.SetGlobalEnabled(true)
	now := time.Now()
	p1End := now.Add(10 * 24 * time.Hour)
	p2End := now.Add(3 * 24 * time.Hour)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}

	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, billingSummaryPath):
			return jsonResp(200, billingData(`{"Packages":[
				{"PackageCode":"P1","CycleTotalCapacity":"100.5","CycleRemainCapacity":"60.25"},
				{"PackageCode":"P2","CycleTotalCapacity":200,"CycleRemainCapacity":40}
			]}`)), nil
		case strings.HasSuffix(r.URL.Path, billingPaidPath):
			// 同一个 PackageCode 出现两期：只认最早的那期，不重复计余额。
			return jsonResp(200, billingData(`{"Accounts":[
				{"PackageCode":"P1","DeductionEndTime":`+fmtMs(p1End.UnixMilli())+`,"CycleCapacityRemainPrecise":"60.25"},
				{"PackageCode":"P1","DeductionEndTime":`+fmtMs(now.Add(40*24*time.Hour).UnixMilli())+`,"CycleCapacityRemainPrecise":"60.25"}
			]}`)), nil
		case strings.HasSuffix(r.URL.Path, billingFreePath):
			return jsonResp(200, billingData(`{"Accounts":[
				{"PackageCode":"P2","CycleEndTime":"`+p2End.In(softRateResetLoc).Format(packageEndLayout)+`","CycleCapacityRemain":40}
			]}`)), nil
		}
		return nil, errors.New("unexpected path " + r.URL.Path)
	})

	remain, total, expiring, earliestAt, earliestRemaining, err := c.globalUserResource(a, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 100 || total != 300 {
		t.Errorf("remain/total=%d/%d want 100/300", remain, total)
	}
	if expiring != 40 {
		t.Errorf("expiring=%d want 40 (P2 三天后到期)", expiring)
	}
	if earliestRemaining != 40 {
		t.Errorf("earliestRemaining=%d want 40", earliestRemaining)
	}
	if d := earliestAt.Sub(p2End); d > time.Second || d < -time.Second {
		t.Errorf("earliestAt=%v want ~%v (diff %v)", earliestAt, p2End, d)
	}
	if len(paths) != 3 {
		t.Errorf("paths=%v want summary+paid+free", paths)
	}
}

func TestGlobalUserResourceSummaryError(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"code":10000,"msg":"boom"}`), nil
	})
	if _, _, _, _, _, err := c.globalUserResource(a, 0); err == nil {
		t.Fatal("summary 失败应返回错误（由调用方回落 CN 路径）")
	}
}

func TestGlobalPackageDetailsToleratesOneFailure(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, billingPaidPath) {
			return jsonResp(500, `{"code":10000,"msg":"nope"}`), nil
		}
		return jsonResp(200, billingData(`{"Accounts":[{"PackageCode":"F1","CycleCapacityRemain":7}]}`)), nil
	})
	got := c.globalPackageDetails(a, nil)
	if len(got) != 1 || got[0].PackageCode != "F1" {
		t.Fatalf("details=%v want 单条 F1（paid 失败不影响 free）", got)
	}
}

// TestGlobalBillingFallsBackToCNPath 断言 summary 失败时仍走原有分页路径，
// 不因国际版新口径失败而丢掉余额（否则该账号会被积分保底当耗尽号挡掉）。
func TestGlobalBillingFallsBackToCNPath(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, billingSummaryPath) {
			return jsonResp(500, `{"code":10000,"msg":"no summary"}`), nil
		}
		// CN 分页路径仍用 data.Response.Data 老信封，故这里必须用 Legacy 构造。
		return jsonResp(200, billingDataLegacy(`{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":500,"CycleCapacityRemain":321,"CycleCapacityUsed":179}
		]}`)), nil
	})

	remain, _, _, _, _, err := c.UserResourceDetailedWithExpiry(a, 0)
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if remain != 321 {
		t.Errorf("remain=%d want 321（回落 CN 分页路径）", remain)
	}
	if len(paths) < 2 || !strings.HasSuffix(paths[0], billingSummaryPath) {
		t.Errorf("paths=%v want summary 先试再回落", paths)
	}
}

// TestSummaryRealShape 用 2026-10-23 抓包的真实响应形状（业务字段直接在 data 下）：
// 上一版只认 data.Response.Data，导致 Packages 恒空 ⇒ 全池 global 号 credits 被写成 0。
func TestSummaryRealShape(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, billingSummaryPath) {
			return jsonResp(200, billingData(`{"Packages":[
				{"PackageCode":"TCACA_code_007","CycleTotalCapacity":"1000","CycleRemainCapacity":"1000","CapacityUnit":"credit"},
				{"PackageCode":"TCACA_code_035","CycleTotalCapacity":"100","CycleRemainCapacity":"100","CapacityUnit":"credits"}
			],"TotalCredits":"1100","SubscriptionPackageName":"Free","IsPaidUser":false}`)), nil
		}
		return jsonResp(200, billingData(`{"Accounts":[],"TotalCount":0}`)), nil
	})
	remain, total, _, _, _, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 1100 || total != 1100 {
		t.Errorf("remain/total=%d/%d want 1100/1100（真实形状）", remain, total)
	}
}

// TestSummaryTotalCreditsFallback Packages 为空但给了 TotalCredits 时用总账兜底。
func TestSummaryTotalCreditsFallback(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, billingSummaryPath) {
			return jsonResp(200, billingData(`{"Packages":[],"TotalCredits":"88"}`)), nil
		}
		return jsonResp(200, billingData(`{"Accounts":[],"TotalCount":0}`)), nil
	})
	remain, total, _, _, _, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 88 || total != 88 {
		t.Errorf("remain/total=%d/%d want 88/88", remain, total)
	}
}

// TestSummaryEmptyIsUnknown 空额度必须报错（让上层回落），不能当成「已知为 0」。
func TestSummaryEmptyIsUnknown(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, billingSummaryPath) {
			return jsonResp(200, billingData(`{"Packages":[],"TotalCredits":"0"}`)), nil
		}
		return jsonResp(200, billingData(`{"Accounts":[],"TotalCount":0}`)), nil
	})
	if _, _, _, _, _, err := c.globalUserResource(a, 0); err == nil {
		t.Fatal("空额度应返回错误（未知 ≠ 0）")
	}
}

// TestSummaryLegacyEnvelope CN 风格信封仍可解（兼容层不丢）。
func TestSummaryLegacyEnvelope(t *testing.T) {
	auth.SetGlobalEnabled(true)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, billingSummaryPath) {
			return jsonResp(200, billingDataLegacy(`{"Packages":[
				{"PackageCode":"P","CycleTotalCapacity":"10","CycleRemainCapacity":"7"}
			]}`)), nil
		}
		return jsonResp(200, billingData(`{"Accounts":[],"TotalCount":0}`)), nil
	})
	remain, total, _, _, _, err := c.globalUserResource(a, 0)
	if err != nil {
		t.Fatalf("globalUserResource: %v", err)
	}
	if remain != 7 || total != 10 {
		t.Errorf("remain/total=%d/%d want 7/10（旧信封）", remain, total)
	}
}

func TestPackageEndAtPrecedence(t *testing.T) {
	ms := time.Date(2026, 11, 1, 12, 0, 0, 0, softRateResetLoc).UnixMilli()
	if got, ok := packageEndAt(ms, "2026-12-01 00:00:00", "2027-01-01 00:00:00"); !ok || got.UnixMilli() != ms {
		t.Errorf("毫秒应优先: %v %v", got, ok)
	}
	if got, ok := packageEndAt(0, "2026-12-01 00:00:00", "2027-01-01 00:00:00"); !ok || got.Format(packageEndLayout) != "2026-12-01 00:00:00" {
		t.Errorf("ExpiredTime 次之: %v %v", got, ok)
	}
	if got, ok := packageEndAt(0, "", "2027-01-01 00:00:00"); !ok || got.Year() != 2027 {
		t.Errorf("CycleEndTime 兜底: %v %v", got, ok)
	}
	if _, ok := packageEndAt(0, "", ""); ok {
		t.Error("全空不应给出到期时刻")
	}
}
