package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"autowheel/internal/wheel"
)

type eodObservation struct {
	Date     string `json:"date"`
	Contract string `json:"contract"`
	Ask      int64  `json:"ask_cents"`
	Path     string `json:"response_path"`
	SHA      string `json:"response_sha256"`
}

// A hash proves which bytes were read, not which contract or price they contain.
// These are EOD reports, without a proven strictly fresh 4pm quote clock.
func validateEODSupplement(o eodObservation, raw json.RawMessage) (int64, error) {
	invalid := errors.New("EOD supplement contract, date or quote does not match pinned response")
	var response struct {
		Items []struct {
			Contract struct {
				Symbol     string      `json:"symbol"`
				Expiration string      `json:"expiration"`
				Right      string      `json:"right"`
				Strike     json.Number `json:"strike"`
			} `json:"contract"`
			Data []struct {
				Created      string      `json:"created"`
				Ask          json.Number `json:"ask"`
				Bid          json.Number `json:"bid"`
				AskSize      int64       `json:"ask_size"`
				BidSize      int64       `json:"bid_size"`
				AskCondition int         `json:"ask_condition"`
				BidCondition int         `json:"bid_condition"`
			} `json:"data"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Items) != 1 || len(response.Items[0].Data) != 1 || len(o.Contract) != 21 {
		return 0, invalid
	}
	contract, row := response.Items[0].Contract, response.Items[0].Data[0]
	kind := "PUT"
	if o.Contract[12] == 'C' {
		kind = "CALL"
	} else if o.Contract[12] != 'P' {
		return 0, invalid
	}
	expiry := "20" + o.Contract[6:8] + "-" + o.Contract[8:10] + "-" + o.Contract[10:12]
	if _, err := time.Parse("2006-01-02", expiry); err != nil {
		return 0, invalid
	}
	strikeMilli, strikeErr := strconv.ParseInt(o.Contract[13:], 10, 64)
	strike, priceErr := wheel.ParseCents(contract.Strike.String())
	ask, askErr := wheel.ParseCents(row.Ask.String())
	bid, bidErr := wheel.ParseCents(row.Bid.String())
	created, dateErr := time.ParseInLocation("2006-01-02T15:04:05", row.Created, wheel.Eastern)
	closeAt, session := wheel.SessionClose(created)
	if strikeErr != nil || priceErr != nil || strike <= 0 || !validMoney(int64(strike)) || strikeMilli != int64(strike)*10 || contract.Symbol != strings.TrimSpace(o.Contract[:6]) || contract.Expiration != expiry || strings.ToUpper(contract.Right) != kind || askErr != nil || bidErr != nil || ask <= 0 || !validMoney(int64(ask)) || bid > ask || int64(ask) != o.Ask || row.AskSize <= 0 || row.BidSize < 0 || row.AskCondition != 50 || row.BidCondition != 50 || dateErr != nil || created.Format("2006-01-02") != o.Date || !session || created.Before(closeAt) {
		return 0, invalid
	}
	return int64(ask), nil
}
