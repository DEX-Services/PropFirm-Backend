package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// BI2X chart datafeed proxy — a straight port of Dex-Backend's
// internal/api/bi2xchart.go. The BI2X data feed
// (https://bitdx-feed-jk3y.onrender.com) exposes a TradingView
// UDF-compatible datafeed at /api/datafeed/* but sends no
// Access-Control-Allow-Origin header at all, so browser requests to it
// directly are CORS-blocked. This backend forwards /bi2x-chart/* to the
// real feed server-to-server (no CORS applies between two servers) and the
// frontend points its datafeed at THIS backend's URL instead. PropFirm
// routes through its own backend rather than calling Dex-Backend directly,
// keeping the same "the browser only talks to the prop-firm backend"
// boundary the /markets and /depth proxies already establish.
const (
	// bi2xFeedBaseURL is the real feed server this proxies to. Same fixed
	// upstream as the exchange's own proxy — not env-configurable for the
	// same reason: specific to one third-party dependency for one asset.
	bi2xFeedBaseURL = "https://bitdx-feed-jk3y.onrender.com"

	// bi2xProxyPrefix is the path prefix this backend exposes to the
	// frontend; everything after it is forwarded verbatim (path + query) to
	// bi2xFeedBaseURL + "/api/datafeed" + that remainder.
	bi2xProxyPrefix = "/bi2x-chart"

	// bi2xUpstreamTimeout is per-attempt. The feed is a free-tier Render
	// service: after ~15 minutes idle it cold-starts, and the first request
	// can take 30s+ (observed live). A single 10s timeout turned every cold
	// start into a 502 on the chart's first history load — so use a longer
	// window and retry once below.
	bi2xUpstreamTimeout = 45 * time.Second

	bi2xAttempts = 2
)

// BI2XChartProxy forwards GET requests under /bi2x-chart/* to the BI2X
// feed's real UDF datafeed at {bi2xFeedBaseURL}/api/datafeed/*. It does not
// interpret, cache, or modify the UDF response body — whatever shape the
// upstream feed returns (config/time/symbols/search/history) passes through
// unchanged.
func BI2XChartProxy(log *slog.Logger) http.HandlerFunc {
	return newBI2XChartProxy(bi2xFeedBaseURL, log)
}

func newBI2XChartProxy(upstreamBase string, log *slog.Logger) http.HandlerFunc {
	client := &http.Client{Timeout: bi2xUpstreamTimeout}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "GET only")
			return
		}

		// Strip our prefix, keep everything after it (including a leading
		// slash for the specific sub-route: /config, /time, /symbols,
		// /search, /history) and re-root it under the upstream's own
		// /api/datafeed path.
		remainder := strings.TrimPrefix(r.URL.Path, bi2xProxyPrefix)
		upstreamURL := upstreamBase + "/api/datafeed" + remainder
		if r.URL.RawQuery != "" {
			upstreamURL += "?" + r.URL.RawQuery
		}

		// Retry once on transport failure: the free-tier feed cold-starts
		// (30s+ first request after idle) and its connection setup can also
		// transiently fail. GETs are safe to re-issue. Only the final
		// attempt's result is surfaced to the browser.
		var resp *http.Response
		var lastErr error
		for attempt := 0; attempt < bi2xAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(500 * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(r.Context(), bi2xUpstreamTimeout)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
			if err != nil {
				cancel()
				writeError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
				return
			}
			resp, lastErr = client.Do(req)
			if lastErr == nil {
				if attempt > 0 {
					log.Warn("bi2x chart proxy: retry succeeded", "path", remainder, "attempt", attempt+1)
				}
				defer resp.Body.Close()
				defer cancel()
				break
			}
			cancel()
			log.Warn("bi2x chart proxy: upstream request failed", "path", remainder, "attempt", attempt+1, "error", lastErr)
		}
		if resp == nil {
			writeError(w, http.StatusBadGateway, "bi2x feed unavailable")
			return
		}

		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Warn("bi2x chart proxy: copying upstream body failed", "path", remainder, "error", err)
		}
		if resp.StatusCode >= 500 {
			log.Warn("bi2x feed returned a server error", "path", remainder, "status", resp.StatusCode)
		}
	}
}
