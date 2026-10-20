package gateway

// The console's two check-in endpoints: press it now, and switch the daily one
// on or off. What came of the last try travels on the quota card itself (see
// checkinView), so the panel needs no request of its own to show a streak.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// checkinView is the check-in block a subscription's quota card renders.
type checkinView struct {
	// Supported is false for every vendor that runs no daily event, which is why
	// the console shows no switch there at all.
	Supported bool `json:"supported"`
	Enabled   bool `json:"enabled"`
	// Today is the Beijing day the record is measured against, computed on the
	// gateway: a console in another zone must not decide for itself that a
	// check-in from 00:30 is yesterday's.
	Today string `json:"today"`
	// Record is the latest answer for this account, from any day. Absent when
	// the account has never been checked in.
	Record *domain.CheckinRecord `json:"record,omitempty"`
}

// checkinViewFor reads the card's block for a provider. Nothing is asked of the
// vendor: this only reports what is already known.
func (s *Server) checkinViewFor(provider domain.Provider) checkinView {
	_, supported := checkinVendorOf(provider)
	view := checkinView{Supported: supported, Enabled: provider.CheckinEnabled, Today: checkinDay(time.Now())}
	if !supported || !s.checkinStoreReady() {
		return view
	}
	accountKey, _ := checkinAccountKey(provider)
	record, found, err := s.checkinStore.CheckinByProvider(provider.ID)
	if err != nil || !found {
		return view
	}
	// A record of a different account — the card was signed in to somebody else
	// since — is not this account's answer, so the card starts over.
	if accountKey != "" && record.AccountKey != accountKey {
		return view
	}
	record.ProviderID = provider.ID
	view.Record = &record
	return view
}

// handleProviderCheckin presses one account's check-in now. It is the console's
// 立即签到 button, and it runs even when the account is disabled or its switch is
// off: the user just asked for this one.
func (s *Server) handleProviderCheckin(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderAccessForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if _, supported := checkinVendorOf(provider); !supported {
		writeOpenAIError(w, http.StatusConflict, "这个订阅的厂商没有每日签到活动")
		return
	}
	if !checkinUsable(provider) {
		writeOpenAIError(w, http.StatusConflict, "这条订阅还没有可签到的账号，先登录")
		return
	}
	if _, _, ran := s.checkinProviderNow(r.Context(), providerID, true); !ran {
		writeOpenAIError(w, http.StatusConflict, "这个账号的签到正在进行中，稍后再试")
		return
	}
	updated, lookupErr := s.router.ProviderByID(providerID)
	if lookupErr != nil {
		updated = provider
	}
	writeJSON(w, http.StatusOK, s.checkinViewFor(updated))
}

// handleProviderCheckinAuto switches the daily check-in of one subscription. Like
// the RTK switch it has its own endpoint, so a partial provider patch cannot
// reset it.
func (s *Server) handleProviderCheckinAuto(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderAccessForUser(w, r, providerID) {
		return
	}
	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, `body must be {"enabled": true|false}`)
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if _, supported := checkinVendorOf(provider); !supported && *payload.Enabled {
		writeOpenAIError(w, http.StatusConflict, "这个订阅的厂商没有每日签到活动")
		return
	}
	updated, err := s.router.SetProviderCheckin(providerID, *payload.Enabled)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	if *payload.Enabled {
		s.logs.AddApp("info", "每日签到已开启", updated.ID)
		// The loop looks once a minute, so waiting for it would leave the card
		// saying "今日未签到" for a minute after the user turned the switch on.
		go s.checkinProviderNow(context.Background(), updated.ID, true)
	} else {
		s.logs.AddApp("info", "每日签到已关闭", updated.ID)
	}
	view := s.checkinViewFor(updated)
	view.Enabled = updated.CheckinEnabled
	writeJSON(w, http.StatusOK, view)
}
