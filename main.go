package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	netmail "net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wbergg/beerfax/internal/fax"
	"github.com/wbergg/beerfax/internal/mail"
)

var errRollNotFound = errors.New("roll not found")

const (
	httpTimeout = 10 * time.Second
	// Transient fetch failures (network errors, 5xx) are retried with linear
	// backoff so a momentary blip doesn't drop a whole day's fax. 404 and other
	// 4xx are terminal and not retried.
	fetchAttempts = 3
	fetchBackoff  = 2 * time.Second

	// Body PNG matches the body region inside fax.convertImage:
	// (faxWidth-100, faxHeight - headerH). headerH for our header is 350.
	bodyWidth  = 1628
	bodyHeight = 1942

	// Pagination: at pointsize 28, line advance is ~33.6px in plain Courier,
	// but lines containing fallback-font glyphs (block elements, box drawing,
	// bullets) render slightly taller. linesPerPage is a conservative initial
	// estimate; actual rendered height is verified against bodyHeight after
	// pagination and pages are re-split if they overflow.
	bodyPointSize = 28
	linesPerPage  = 50
	// Pixel budget for rendered body text (canvas height minus top margin).
	bodyHeightBudget = bodyHeight - 20
)

type appConfig struct {
	APIURL         string          `json:"api_url"`
	Telephony      telephonyConfig `json:"telephony"`
	Email          emailConfig     `json:"email"`
	FaxSpoolPath   string          `json:"fax_spool_path"`
	FaxStoragePath string          `json:"fax_storage_path"`
}

// emailConfig is optional: with no recipients configured the email step is
// skipped entirely and beerfax behaves as before.
type emailConfig struct {
	To   []string `json:"to"`
	From string   `json:"from"`
}

type telephonyConfig struct {
	DestExt  int    `json:"dest_ext"`
	CallerID string `json:"caller_id"`
	FromName string `json:"from_name"`
}

type apiResponse struct {
	EventName    string        `json:"eventName"`
	Participants []Participant `json:"participants"`
	State        stateBlock    `json:"state"`
}

type Participant struct {
	UserID   int    `json:"userId"`
	Username string `json:"username"`
}

type stateBlock struct {
	PoolCount  int             `json:"poolCount"`
	TotalCount int             `json:"totalCount"`
	Consumed   []ConsumedEvent `json:"consumed"`
	Vetoed     []VetoedEvent   `json:"vetoed"`
}

type ConsumedEvent struct {
	ID              int64     `json:"id"`
	ProductNameBold string    `json:"productNameBold"`
	ProductNameThin string    `json:"productNameThin"`
	ProducerName    string    `json:"producerName"`
	Country         string    `json:"country"`
	ConsumedByName  string    `json:"consumedByName"`
	ConsumedAt      time.Time `json:"consumedAt"`
	Vetoed          bool      `json:"vetoed"`
	AlcoholPercent  float64   `json:"alcoholPercent"`
	Volume          int       `json:"volume"`
	Style           string    `json:"style"`
	DecisionSeconds float64   `json:"decisionSeconds"`
}

type VetoedEvent struct {
	ProductNameBold string    `json:"productNameBold"`
	ProductNameThin string    `json:"productNameThin"`
	VetoedByName    string    `json:"vetoedByName"`
	VetoedAt        time.Time `json:"vetoedAt"`
	DecisionSeconds float64   `json:"decisionSeconds"`
}

type Summary struct {
	Events       []ConsumedEvent
	AllEvents    []ConsumedEvent
	Participants []Participant
	Vetoes       []VetoedEvent
	Stats        string
	Start        time.Time
	End          time.Time
}

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "config/config.json", "path to config.json")
	dryRun := flag.Bool("dry-run", false, "render the TIFF and a viewable PDF, then exit without archiving or queueing a fax")
	dateFlag := flag.String("date", "", "use a fixed day window (YYYY-MM-DD, 04:00→04:00 next day) instead of in-progress today; archives the TIFF but skips queueing the fax")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Printf("config: %v", err)
		return 1
	}

	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		log.Printf("tz: %v", err)
		return 1
	}
	var start, end time.Time
	if *dateFlag != "" {
		d, err := time.ParseInLocation("2006-01-02", *dateFlag, loc)
		if err != nil {
			log.Printf("date: %v", err)
			return 1
		}
		start = time.Date(d.Year(), d.Month(), d.Day(), 4, 0, 0, 0, loc)
		end = start.AddDate(0, 0, 1)
	} else {
		start, end = dayWindow(time.Now(), loc)
	}
	log.Printf("window: %s .. %s", start.Format(time.RFC3339), end.Format(time.RFC3339))

	resp, err := fetchRoll(cfg.APIURL)
	if err != nil {
		if errors.Is(err, errRollNotFound) {
			log.Printf("api: 404, skipping")
			return 0
		}
		log.Printf("fetch: %v", err)
		return 2
	}

	events := filterWindow(resp.State.Consumed, start, end)
	if len(events) == 0 {
		log.Printf("no events in window, skipping")
		return 0
	}

	prevEvents := filterWindow(resp.State.Consumed, start.AddDate(0, 0, -1), start)
	delta := deltaLine(events, prevEvents)

	summary := applyLogic(events, resp, start, end)

	jobID := time.Now().UnixNano()
	storageAbs, err := filepath.Abs(cfg.FaxStoragePath)
	if err != nil {
		log.Printf("storage abs: %v", err)
		return 4
	}
	jobDir := filepath.Join(storageAbs, "beerfax", strconv.FormatInt(jobID, 10))
	if err := os.MkdirAll(jobDir, 0775); err != nil {
		log.Printf("mkdir: %v", err)
		return 4
	}

	body := renderBody(summary, loc)
	if dayList := dailyConsumption(resp.State.Consumed, start, end, loc); dayList != "" {
		body = dayList + "\n" + body
	}
	pages := paginate(body, linesPerPage)
	final := make([]string, 0, len(pages))
	for _, p := range pages {
		final = append(final, splitToFitHeight(p, bodyHeightBudget)...)
	}
	pages = final

	message := summary.Stats
	if delta != "" {
		message += "\nvs yesterday: " + delta
	}
	baseSubject := fmt.Sprintf("%s Daily Roll %s", truncate(resp.EventName, 60), start.Format("2006-01-02"))
	emailBody := message + "\n\n" + body + "\nFull report attached as PDF.\n"
	pdfName := fmt.Sprintf("beerfax-%s.pdf", start.Format("2006-01-02"))

	pageTiffs := make([]string, 0, len(pages))
	for i, pageText := range pages {
		pagePNG := filepath.Join(jobDir, fmt.Sprintf("page_%d.png", i+1))
		if err := generatePNG(pagePNG, pageText); err != nil {
			log.Printf("convert page %d: %v", i+1, err)
			return 5
		}
		subject := baseSubject
		if len(pages) > 1 {
			subject = fmt.Sprintf("%s (%d/%d)", baseSubject, i+1, len(pages))
		}
		pageTiff, err := fax.ConvertToTIFF(pagePNG, jobDir, fax.FaxHeader{
			To:      strconv.Itoa(cfg.Telephony.DestExt),
			From:    cfg.Telephony.FromName,
			Subject: subject,
			Message: message,
		})
		if err != nil {
			log.Printf("tiff page %d: %v", i+1, err)
			return 6
		}
		pageTiffs = append(pageTiffs, pageTiff)
	}

	tiffPath := pageTiffs[0]
	if len(pageTiffs) > 1 {
		tiffPath = filepath.Join(jobDir, "body.tiff")
		if err := combineTIFFs(pageTiffs, tiffPath); err != nil {
			log.Printf("combine tiff: %v", err)
			return 6
		}
	}

	if *dryRun {
		pdfPath := filepath.Join(jobDir, pdfName)
		if _, err := fax.ConvertTIFFToPDF(pdfPath, pageTiffs...); err != nil {
			log.Printf("dry-run pdf: %v", err)
			return 6
		}
		if err := sendReportEmail(cfg.Email, "[DRY-RUN] "+baseSubject, emailBody, pdfPath); err != nil {
			log.Printf("dry-run email: %v", err)
			return 8
		}
		log.Printf("dry-run: tiff at %s, pdf at %s (%d page(s), events=%d, jobDir=%s)", tiffPath, pdfPath, len(pageTiffs), len(events), jobDir)
		return 0
	}

	archiveDir := filepath.Join(storageAbs, "archive")
	if err := os.MkdirAll(archiveDir, 0775); err != nil {
		log.Printf("archive mkdir: %v", err)
		return 6
	}
	archiveStamp := time.Now().In(loc).Format("2006-01-02-150405")
	if *dateFlag != "" {
		archiveStamp = start.In(loc).Format("2006-01-02") + "-replay-" + time.Now().In(loc).Format("150405")
	}
	archivePath := filepath.Join(archiveDir, archiveStamp+".tiff")
	if err := copyFile(tiffPath, archivePath); err != nil {
		log.Printf("archive copy: %v", err)
		return 6
	}

	if *dateFlag != "" {
		pdfPath := strings.TrimSuffix(archivePath, filepath.Ext(archivePath)) + ".pdf"
		if _, err := fax.ConvertTIFFToPDF(pdfPath, archivePath); err != nil {
			log.Printf("date replay pdf: %v", err)
			return 6
		}
		if err := sendReportEmail(cfg.Email, baseSubject+" (replay)", emailBody, pdfPath); err != nil {
			log.Printf("date replay email: %v", err)
			return 8
		}
		log.Printf("date replay: archived tiff at %s, pdf at %s (events=%d, jobDir=%s)", archivePath, pdfPath, len(events), jobDir)
		return 0
	}

	// The PDF only feeds the email report, so a conversion failure must not
	// block the fax: log it and queue anyway.
	pdfPath := filepath.Join(jobDir, pdfName)
	if _, err := fax.ConvertTIFFToPDF(pdfPath, pageTiffs...); err != nil {
		log.Printf("report pdf: %v (continuing without email)", err)
		pdfPath = ""
	}

	callFile, err := fax.WriteCallFile(cfg.FaxSpoolPath, int(jobID), cfg.Telephony.DestExt, tiffPath, jobDir, cfg.Telephony.CallerID)
	if err != nil {
		log.Printf("call file: %v", err)
		return 7
	}
	log.Printf("queued fax: %s (events=%d, jobDir=%s)", callFile, len(events), jobDir)

	if pdfPath != "" {
		if err := sendReportEmail(cfg.Email, baseSubject, emailBody, pdfPath); err != nil {
			log.Printf("email: %v (fax was still queued)", err)
			return 8
		}
	}
	return 0
}

// sendReportEmail emails the report text with the PDF attached via msmtp.
// With no recipients configured it is a silent no-op so the fax pipeline
// works without an email setup.
func sendReportEmail(cfg emailConfig, subject, textBody, pdfPath string) error {
	if len(cfg.To) == 0 {
		log.Printf("email: no recipients configured, skipping")
		return nil
	}
	if err := mail.Send(mail.Message{
		From:        cfg.From,
		To:          cfg.To,
		Subject:     subject,
		Body:        textBody,
		Attachments: []string{pdfPath},
	}); err != nil {
		return err
	}
	log.Printf("email: sent %q to %s", subject, strings.Join(cfg.To, ", "))
	return nil
}

func loadConfig(path string) (*appConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &appConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.APIURL == "" || cfg.FaxSpoolPath == "" || cfg.FaxStoragePath == "" {
		return nil, fmt.Errorf("missing api_url, fax_spool_path or fax_storage_path in %s", path)
	}
	if cfg.Telephony.DestExt == 0 || cfg.Telephony.CallerID == "" || cfg.Telephony.FromName == "" {
		return nil, fmt.Errorf("missing telephony.dest_ext, telephony.caller_id or telephony.from_name in %s", path)
	}
	// msmtp gets recipients as bare envelope addresses, so each entry must be
	// a full user@domain (no display-name form, no local usernames).
	for i, addr := range cfg.Email.To {
		parsed, err := netmail.ParseAddress(addr)
		if err != nil || parsed.Address != addr || !strings.Contains(addr, "@") {
			return nil, fmt.Errorf("email.to[%d]: %q is not a plain user@domain address", i, addr)
		}
	}
	if cfg.Email.From != "" {
		if _, err := netmail.ParseAddress(cfg.Email.From); err != nil {
			return nil, fmt.Errorf("email.from: %q is not a valid address: %v", cfg.Email.From, err)
		}
	}
	return cfg, nil
}

// dayWindow returns the in-progress 04:00→now Europe/Stockholm window:
// start is the most recent 04:00 cutoff (today's if we're past it, otherwise
// yesterday's), end is now. AddDate (not Add(-24h)) is used so DST transition
// days produce a 23h or 25h span correctly.
func dayWindow(now time.Time, loc *time.Location) (start, end time.Time) {
	nowLocal := now.In(loc)
	y, m, d := nowLocal.Date()
	todayCutoff := time.Date(y, m, d, 4, 0, 0, 0, loc)
	start = todayCutoff
	if nowLocal.Before(todayCutoff) {
		start = todayCutoff.AddDate(0, 0, -1)
	}
	end = nowLocal
	return start, end
}

func fetchRoll(apiURL string) (*apiResponse, error) {
	client := &http.Client{Timeout: httpTimeout}
	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		if attempt > 1 {
			delay := fetchBackoff * time.Duration(attempt-1)
			log.Printf("fetch: attempt %d/%d failed (%v), retrying in %s", attempt-1, fetchAttempts, lastErr, delay)
			time.Sleep(delay)
		}
		out, err := fetchRollOnce(client, apiURL)
		if err == nil {
			return out, nil
		}
		// 404 and other 4xx are terminal; only transient failures are retried.
		if errors.Is(err, errRollNotFound) || errors.Is(err, errClientStatus) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

var errClientStatus = errors.New("client error status")

func fetchRollOnce(client *http.Client, apiURL string) (*apiResponse, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "beerfax/1.0")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, errRollNotFound
	}
	if res.StatusCode != http.StatusOK {
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			return nil, fmt.Errorf("status %d: %w", res.StatusCode, errClientStatus)
		}
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var out apiResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func filterWindow(events []ConsumedEvent, start, end time.Time) []ConsumedEvent {
	out := make([]ConsumedEvent, 0, len(events))
	for _, e := range events {
		if e.ConsumedAt.Compare(start) >= 0 && e.ConsumedAt.Before(end) {
			out = append(out, e)
		}
	}
	return out
}

// deltaLine renders a "+N beers, -M countries" comparison of today's events vs
// the prior day's events. Returns "" when both sides are empty.
func deltaLine(today, prev []ConsumedEvent) string {
	if len(today) == 0 && len(prev) == 0 {
		return ""
	}
	beerDelta := len(today) - len(prev)
	countryDelta := distinctCountries(today) - distinctCountries(prev)
	beerLabel := "beers"
	if abs(beerDelta) == 1 {
		beerLabel = "beer"
	}
	countryLabel := "countries"
	if abs(countryDelta) == 1 {
		countryLabel = "country"
	}
	return fmt.Sprintf("%+d %s, %+d %s", beerDelta, beerLabel, countryDelta, countryLabel)
}

// dailyConsumption lists each 04:00-window day from the first consumed event
// through the current window's day, with a day-over-day diff from the second
// line on:
//
//	2026-07-06 - 45 beers consumed
//	2026-07-07 - 62(+17) beers consumed
//
// Days between with no events show as 0. Events at or after windowEnd are
// ignored so a --date replay reproduces that day's view. Returns "" when the
// event has only spanned a single day so far, since the count would just
// repeat the stats line.
func dailyConsumption(all []ConsumedEvent, windowStart, windowEnd time.Time, loc *time.Location) string {
	windowDay := func(t time.Time) time.Time {
		tl := t.In(loc).Add(-4 * time.Hour)
		return time.Date(tl.Year(), tl.Month(), tl.Day(), 4, 0, 0, 0, loc)
	}
	counts := map[time.Time]int{}
	var first time.Time
	for _, e := range all {
		if !e.ConsumedAt.Before(windowEnd) {
			continue
		}
		d := windowDay(e.ConsumedAt)
		counts[d]++
		if first.IsZero() || d.Before(first) {
			first = d
		}
	}
	last := windowDay(windowStart)
	if first.IsZero() || first.Equal(last) {
		return ""
	}
	var b strings.Builder
	prev, havePrev := 0, false
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		n := counts[d]
		label := "beers"
		if n == 1 {
			label = "beer"
		}
		if havePrev {
			fmt.Fprintf(&b, "%s - %d(%+d) %s consumed\n", d.Format("2006-01-02"), n, n-prev, label)
		} else {
			fmt.Fprintf(&b, "%s - %d %s consumed\n", d.Format("2006-01-02"), n, label)
		}
		prev, havePrev = n, true
	}
	return b.String()
}

func distinctCountries(events []ConsumedEvent) int {
	seen := map[string]struct{}{}
	for _, e := range events {
		c := strings.TrimSpace(e.Country)
		if c == "" {
			continue
		}
		seen[c] = struct{}{}
	}
	return len(seen)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// applyLogic is the placeholder transform — real scoring/filtering goes here later.
func applyLogic(events []ConsumedEvent, resp *apiResponse, start, end time.Time) Summary {
	vetoes := make([]VetoedEvent, 0, len(resp.State.Vetoed))
	for _, v := range resp.State.Vetoed {
		if v.VetoedAt.Compare(start) >= 0 && v.VetoedAt.Before(end) {
			vetoes = append(vetoes, v)
		}
	}
	return Summary{
		Events:       events,
		AllEvents:    resp.State.Consumed,
		Participants: resp.Participants,
		Vetoes:       vetoes,
		Start:        start,
		End:          end,
		Stats: fmt.Sprintf("%d events · %d/%d consumed · %d in pool",
			len(events), resp.State.TotalCount-resp.State.PoolCount, resp.State.TotalCount, resp.State.PoolCount),
	}
}

// renderBody groups events by user (chronological within each section), sections ordered
// most-active-first with alphabetical tiebreak.
func renderBody(s Summary, loc *time.Location) string {
	byUser := map[string][]ConsumedEvent{}
	for _, e := range s.Events {
		name := e.ConsumedByName
		if name == "" {
			name = "unknown"
		}
		byUser[name] = append(byUser[name], e)
	}

	type section struct {
		name   string
		events []ConsumedEvent
	}
	sections := make([]section, 0, len(byUser))
	for name, evs := range byUser {
		sort.Slice(evs, func(i, j int) bool { return evs[i].ConsumedAt.Before(evs[j].ConsumedAt) })
		sections = append(sections, section{name, evs})
	}
	sort.Slice(sections, func(i, j int) bool {
		if len(sections[i].events) != len(sections[j].events) {
			return len(sections[i].events) > len(sections[j].events)
		}
		return sections[i].name < sections[j].name
	})

	var b strings.Builder
	leaderPeak, leaderNameW := 0, 0
	for _, sec := range sections {
		if len(sec.events) > leaderPeak {
			leaderPeak = len(sec.events)
		}
		if len(sec.name) > leaderNameW {
			leaderNameW = len(sec.name)
		}
	}
	rows := make([]string, 0, len(sections))
	innerW := utf8.RuneCountInString("Leaderboard")
	rankW := len(strconv.Itoa(len(sections))) + 2
	for i, sec := range sections {
		prefix := fmt.Sprintf("%*s", rankW, fmt.Sprintf("%d.", i+1))
		row := fmt.Sprintf("%s %-*s %-30s %d",
			prefix, leaderNameW, sec.name, barChars(len(sec.events), leaderPeak, 30), len(sec.events))
		if w := utf8.RuneCountInString(row); w > innerW {
			innerW = w
		}
		rows = append(rows, row)
	}
	border := "+" + strings.Repeat("-", innerW+2) + "+"
	b.WriteString(border + "\n")
	fmt.Fprintf(&b, "| %-*s |\n", innerW, "Leaderboard")
	b.WriteString(border + "\n")
	for _, row := range rows {
		fmt.Fprintf(&b, "| %-*s |\n", innerW, row)
	}
	b.WriteString(border + "\n\n")

	used, notUsed := vetoStatus(s.Participants, s.Vetoes)
	if len(used) > 0 {
		b.WriteString("Used vetos: ")
		for i, u := range used {
			if i > 0 {
				b.WriteString(", ")
			}
			if u.count > 1 {
				fmt.Fprintf(&b, "%s (%d)", u.name, u.count)
			} else {
				b.WriteString(u.name)
			}
		}
		b.WriteString("\n")
		details := vetoDetails(s.Vetoes)
		nameW := 0
		for _, d := range details {
			if len(d.user) > nameW {
				nameW = len(d.user)
			}
		}
		for _, d := range details {
			fmt.Fprintf(&b, "  %-*s  %s - %s\n", nameW, d.user, d.at.In(loc).Format("2006-01-02 15:04"), d.beer)
		}
	}
	if len(used) > 0 && len(notUsed) > 0 {
		b.WriteString("\n")
	}
	if len(notUsed) > 0 {
		fmt.Fprintf(&b, "Not used vetos: %s\n", strings.Join(notUsed, ", "))
	}
	if len(used) > 0 || len(notUsed) > 0 {
		b.WriteString("\n")
	}

	if stats := renderSingleStats(s.Events, s.Vetoes, s.Start, s.End, loc); stats != "" {
		b.WriteString(stats)
		b.WriteString("\n")
	}

	if hist := renderHourHistogram(s.Events, loc); hist != "" {
		b.WriteString(hist)
		b.WriteString("\n")
	}

	if top := topStrongest(s.Events, 3); len(top) > 0 {
		b.WriteString("Top3 strongest:\n")
		for i, ev := range top {
			label := sanitize(ev.ProductNameBold)
			if ev.ProductNameThin != "" {
				label += " — " + sanitize(ev.ProductNameThin)
			}
			line := fmt.Sprintf("  %d. %.1f%%  %s · %s",
				i+1, ev.AlcoholPercent, label, sanitize(ev.ConsumedByName))
			b.WriteString(truncate(line, 110))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if styles := topUniqueStyles(s.AllEvents, 6); len(styles) > 0 {
		b.WriteString("Most unique styles (all-time):\n")
		peak, nameW := 0, 0
		for _, st := range styles {
			if st.count > peak {
				peak = st.count
			}
			if len(st.user) > nameW {
				nameW = len(st.user)
			}
		}
		for i, st := range styles {
			fmt.Fprintf(&b, "  %d. %-*s %-30s %d\n",
				i+1, nameW, st.user, barChars(st.count, peak, 30), st.count)
		}
		b.WriteString("\n")
	}

	if perUser := topStylePerUser(s.AllEvents); len(perUser) > 0 {
		b.WriteString("Most consumed style per user (all-time):\n")
		for _, u := range perUser {
			fmt.Fprintf(&b, "  %s - %s (%d)\n", u.user, strings.Join(u.styles, ", "), u.count)
		}
		b.WriteString("\n")
	}

	if stamps := countryTour(s.Events); len(stamps) > 0 {
		fmt.Fprintf(&b, "Country tour: %d stamps today\n", distinctCountries(s.Events))
		peak, nameW := 0, 0
		for _, u := range stamps {
			if u.count > peak {
				peak = u.count
			}
			if len(u.user) > nameW {
				nameW = len(u.user)
			}
		}
		for i, u := range stamps {
			fmt.Fprintf(&b, "  %d. %-*s %-30s %d\n",
				i+1, nameW, u.user, barChars(u.count, peak, 30), u.count)
		}
		b.WriteString("\n")
	}

	if tbl := renderAbvTable(s.Events); tbl != "" {
		b.WriteString(tbl)
		b.WriteString("\n")
	}

	for i, sec := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s (%d):\n", sec.name, len(sec.events))
		for _, ev := range sec.events {
			label := sanitize(ev.ProductNameBold)
			if ev.ProductNameThin != "" {
				label += " — " + sanitize(ev.ProductNameThin)
			}
			abv := ""
			if ev.AlcoholPercent > 0 {
				abv = fmt.Sprintf(" %.1f%%", ev.AlcoholPercent)
			}
			line := fmt.Sprintf("  %s  %s%s",
				ev.ConsumedAt.In(loc).Format("15:04"),
				label,
				abv,
			)
			b.WriteString(truncate(line, 110))
			b.WriteString("\n")
		}
	}
	return b.String()
}

type styleCount struct {
	user  string
	count int
}

type userStyle struct {
	user   string
	styles []string
	count  int
}

// countryTour returns each user's distinct-country count for the given events,
// ordered by count desc with alphabetical tiebreak. Users with no countries
// are skipped.
func countryTour(events []ConsumedEvent) []styleCount {
	byUser := map[string]map[string]struct{}{}
	for _, e := range events {
		c := strings.TrimSpace(e.Country)
		u := e.ConsumedByName
		if u == "" || c == "" {
			continue
		}
		set, ok := byUser[u]
		if !ok {
			set = map[string]struct{}{}
			byUser[u] = set
		}
		set[c] = struct{}{}
	}
	out := make([]styleCount, 0, len(byUser))
	for u, set := range byUser {
		out = append(out, styleCount{u, len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].user < out[j].user
	})
	return out
}

// topStylePerUser returns each user's most-consumed style(s) across all events.
// All styles tied for first are returned (alphabetical) along with their shared
// count. Rows ordered by count desc with alphabetical tiebreak; users with no
// styled events are skipped.
func topStylePerUser(events []ConsumedEvent) []userStyle {
	byUser := map[string]map[string]int{}
	for _, e := range events {
		st := strings.TrimSpace(e.Style)
		u := e.ConsumedByName
		if u == "" || st == "" {
			continue
		}
		m, ok := byUser[u]
		if !ok {
			m = map[string]int{}
			byUser[u] = m
		}
		m[st]++
	}
	out := make([]userStyle, 0, len(byUser))
	for u, m := range byUser {
		bestCount := 0
		var bestStyles []string
		for s, c := range m {
			switch {
			case c > bestCount:
				bestCount = c
				bestStyles = []string{s}
			case c == bestCount:
				bestStyles = append(bestStyles, s)
			}
		}
		sort.Strings(bestStyles)
		out = append(out, userStyle{u, bestStyles, bestCount})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].user < out[j].user
	})
	return out
}

// topUniqueStyles returns up to n users ranked by the number of distinct beer
// styles they have consumed across all events. Ties broken alphabetically.
func topUniqueStyles(events []ConsumedEvent, n int) []styleCount {
	byUser := map[string]map[string]struct{}{}
	for _, e := range events {
		u := e.ConsumedByName
		st := strings.TrimSpace(e.Style)
		if u == "" || st == "" {
			continue
		}
		set, ok := byUser[u]
		if !ok {
			set = map[string]struct{}{}
			byUser[u] = set
		}
		set[st] = struct{}{}
	}
	out := make([]styleCount, 0, len(byUser))
	for u, set := range byUser {
		out = append(out, styleCount{u, len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].user < out[j].user
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// renderAbvTable produces a per-user table: lowest/highest/avg ABV and total
// liters consumed. Rows ordered most-active-first, alphabetical tiebreak.
func renderAbvTable(events []ConsumedEvent) string {
	type acc struct {
		user   string
		count  int
		low    float64
		high   float64
		sumAbv float64
		abvN   int
		liters float64
	}
	byUser := map[string]*acc{}
	for _, e := range events {
		u := e.ConsumedByName
		if u == "" {
			continue
		}
		r := byUser[u]
		if r == nil {
			r = &acc{user: u}
			byUser[u] = r
		}
		r.count++
		if e.AlcoholPercent > 0 {
			if r.abvN == 0 || e.AlcoholPercent < r.low {
				r.low = e.AlcoholPercent
			}
			if r.abvN == 0 || e.AlcoholPercent > r.high {
				r.high = e.AlcoholPercent
			}
			r.sumAbv += e.AlcoholPercent
			r.abvN++
		}
		r.liters += float64(e.Volume) / 1000.0
	}
	if len(byUser) == 0 {
		return ""
	}

	rows := make([]*acc, 0, len(byUser))
	for _, r := range byUser {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].user < rows[j].user
	})

	nameW := len("user")
	for _, r := range rows {
		if len(r.user) > nameW {
			nameW = len(r.user)
		}
	}

	const barW = 20
	const scaleMin = 4.2
	const scaleMax = 7.5

	var b strings.Builder
	b.WriteString("Per person:\n")
	fmt.Fprintf(&b, "  %-*s  ABV (4.2-7.5%%)%-*s    low    high   avg    liters\n",
		nameW, "user", barW-len("ABV (4.2-7.5%)"), "")
	for _, r := range rows {
		low, high, avg := "-", "-", "-"
		bar := strings.Repeat(" ", barW)
		if r.abvN > 0 {
			avgVal := r.sumAbv / float64(r.abvN)
			low = fmt.Sprintf("%.1f%%", r.low)
			high = fmt.Sprintf("%.1f%%", r.high)
			avg = fmt.Sprintf("%.1f%%", avgVal)
			bar = abvRangeBar(r.low, r.high, avgVal, scaleMin, scaleMax, barW)
		}
		fmt.Fprintf(&b, "  %-*s  %s   %-6s %-6s %-6s %.2fL\n",
			nameW, r.user, bar, low, high, avg, r.liters)
	}
	return b.String()
}

// abvRangeBar draws a fixed-scale span from low to high with the average
// marked, as e.g. "  ●━━━━┃━━━●        ". Values outside [scaleMin, scaleMax]
// are clamped to the edges.
func abvRangeBar(low, high, avg, scaleMin, scaleMax float64, width int) string {
	if scaleMax <= scaleMin || width <= 0 {
		return strings.Repeat(" ", width)
	}
	span := scaleMax - scaleMin
	bar := []rune(strings.Repeat(" ", width))
	pos := func(v float64) int {
		return clampInt(int((v-scaleMin)*float64(width)/span), 0, width-1)
	}
	lo, hi, av := pos(low), pos(high), pos(avg)
	if hi < lo {
		hi = lo
	}
	for i := lo; i <= hi; i++ {
		bar[i] = '─'
	}
	bar[lo] = '●'
	bar[hi] = '●'
	bar[av] = '|'
	return string(bar)
}

// renderHourHistogram draws a stacked ASCII bar chart of events per hour
// spanning the earliest..latest hour, plus a "peak hour" line.
func renderHourHistogram(events []ConsumedEvent, loc *time.Location) string {
	if len(events) == 0 {
		return ""
	}
	counts := map[int]int{}
	minHour, maxHour := 24, -1
	for _, e := range events {
		h := e.ConsumedAt.In(loc).Hour()
		counts[h]++
		if h < minHour {
			minHour = h
		}
		if h > maxHour {
			maxHour = h
		}
	}
	peakHour, peakCount := -1, 0
	for h, c := range counts {
		if c > peakCount || (c == peakCount && h < peakHour) {
			peakHour, peakCount = h, c
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Activity by hour (%02d-%02d):\n", minHour, maxHour)
	if peakCount > 0 {
		for row := peakCount; row >= 1; row-- {
			line := []byte("  ")
			for h := minHour; h <= maxHour; h++ {
				if h > minHour {
					line = append(line, ' ')
				}
				if counts[h] >= row {
					line = append(line, '#')
				} else {
					line = append(line, ' ')
				}
			}
			b.WriteString(strings.TrimRight(string(line), " "))
			b.WriteByte('\n')
		}
		tens := []byte("  ")
		units := []byte("  ")
		for h := minHour; h <= maxHour; h++ {
			if h > minHour {
				tens = append(tens, ' ')
				units = append(units, ' ')
			}
			tens = append(tens, '0'+byte(h/10))
			units = append(units, '0'+byte(h%10))
		}
		b.Write(tens)
		b.WriteByte('\n')
		b.Write(units)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "Peak hour (per clock hour): %02d:00 (%d)\n", peakHour, peakCount)
	return b.String()
}

// renderSingleStats produces single-line stats: fastest/slowest gap between
// consecutive beers for any one user, the busiest 60-minute sliding window,
// and quickest veto reaction (vetoedAt minus the prior timeline event).
func renderSingleStats(events []ConsumedEvent, vetoes []VetoedEvent, start, now time.Time, loc *time.Location) string {
	firsts := firstToDrink(events)
	fastUser, fastGap, hasFast, slowUser, slowGap, hasSlow := userGapExtremes(events)
	phStart, phCount, hasPH := powerHour(events)
	streaks := longestHourStreak(events, loc)
	hasStreak := len(streaks) > 0
	beerStreaks := longestBeerStreak(events, time.Hour)
	hasBeerStreak := len(beerStreaks) > 0
	qAcceptUser, qAcceptBeer, qAcceptGap, hasQAccept, sAcceptUser, sAcceptBeer, sAcceptGap, hasSAccept := acceptDecisionExtremes(events)
	qVetoUser, qVetoBeer, qVetoGap, hasQVeto, sVetoUser, sVetoBeer, sVetoGap, hasSVeto := vetoDecisionExtremes(vetoes)
	if len(firsts) == 0 && !hasFast && !hasSlow && !hasPH && !hasStreak && !hasBeerStreak &&
		!hasQAccept && !hasSAccept && !hasQVeto && !hasSVeto {
		return ""
	}
	var b strings.Builder
	if len(firsts) > 0 {
		const timelineW = 36
		startHour := start.In(loc).Format("15.04")
		endHour := start.In(loc).Add(24 * time.Hour).Format("15.04")
		nameW := 0
		for _, f := range firsts {
			if len(f.user) > nameW {
				nameW = len(f.user)
			}
		}
		fmt.Fprintf(&b, "First to drink (%s → %s):\n", startHour, endHour)
		for i, f := range firsts {
			elapsed := f.at.Sub(start).Hours()
			pos := clampInt(int(elapsed*float64(timelineW)/24.0), 0, timelineW-1)
			row := []rune(strings.Repeat("─", timelineW))
			row[pos] = '●'
			fmt.Fprintf(&b, "  %d. %s ├%s┤ %s  %-*s  %s\n",
				i+1, startHour, string(row), endHour,
				nameW, f.user, f.at.In(loc).Format("15:04"))
		}
		b.WriteString("\n")
	}
	if hasFast {
		fmt.Fprintf(&b, "Fastest gap between beers: %s — %s\n\n", formatDuration(fastGap), fastUser)
	}
	if hasSlow {
		fmt.Fprintf(&b, "Longest gap between beers: %s — %s\n\n", formatDuration(slowGap), slowUser)
	}
	if hasPH {
		end := phStart.Add(59 * time.Minute)
		fmt.Fprintf(&b, "Power hour (busiest hour any time): %s–%s (%d beers)\n\n",
			phStart.In(loc).Format("15:04"), end.In(loc).Format("15:04"), phCount)
	}
	if hasStreak {
		nowLocal := now.In(loc)
		nowBucket := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), nowLocal.Hour(), 0, 0, 0, loc)
		timeRange := func(s hourStreak) string {
			return fmt.Sprintf("%s–%s",
				s.startHour.Format("15:04"),
				s.endHour.Add(59*time.Minute).Format("15:04"))
		}
		activeMarker := func(s hourStreak) string {
			if s.endHour.Equal(nowBucket) {
				return " (active)"
			}
			return ""
		}
		if len(streaks) == 1 {
			s := streaks[0]
			fmt.Fprintf(&b, "Hour streak (consecutive hours with a beer): %dh (%s) — %s%s\n\n",
				s.hours, timeRange(s), s.user, activeMarker(s))
		} else {
			b.WriteString("Hour streak (consecutive hours with a beer):\n")
			nameW, hourW := 0, 0
			for _, s := range streaks {
				if len(s.user) > nameW {
					nameW = len(s.user)
				}
				if w := len(strconv.Itoa(s.hours)); w > hourW {
					hourW = w
				}
			}
			for i, s := range streaks {
				fmt.Fprintf(&b, "  %d. %*dh  %-*s  %s%s\n",
					i+1, hourW, s.hours, nameW, s.user, timeRange(s), activeMarker(s))
			}
			b.WriteString("\n")
		}
	}
	if hasBeerStreak {
		timeRange := func(s beerStreak) string {
			return fmt.Sprintf("%s–%s",
				s.start.In(loc).Format("15:04"),
				s.end.In(loc).Format("15:04"))
		}
		activeMarker := func(s beerStreak) string {
			if now.Sub(s.end) <= time.Hour {
				return " (active)"
			}
			return ""
		}
		if len(beerStreaks) == 1 {
			s := beerStreaks[0]
			fmt.Fprintf(&b, "Drinking streak (beers within 60 min of each other): %d beers (%s) — %s%s\n\n",
				s.count, timeRange(s), s.user, activeMarker(s))
		} else {
			b.WriteString("Drinking streak (beers within 60 min of each other):\n")
			nameW, countW := 0, 0
			for _, s := range beerStreaks {
				if len(s.user) > nameW {
					nameW = len(s.user)
				}
				if w := len(strconv.Itoa(s.count)); w > countW {
					countW = w
				}
			}
			for i, s := range beerStreaks {
				fmt.Fprintf(&b, "  %d. %*d beers  %-*s  %s%s\n",
					i+1, countW, s.count, nameW, s.user, timeRange(s), activeMarker(s))
			}
			b.WriteString("\n")
		}
	}
	if hasQAccept {
		fmt.Fprintf(&b, "Quickest accept: %s — %s (%s)\n", formatDuration(qAcceptGap), qAcceptUser, qAcceptBeer)
	}
	if hasSAccept && (sAcceptGap != qAcceptGap || sAcceptUser != qAcceptUser) {
		fmt.Fprintf(&b, "Slowest accept: %s — %s (%s)\n", formatDuration(sAcceptGap), sAcceptUser, sAcceptBeer)
	}
	if hasQVeto {
		fmt.Fprintf(&b, "Quickest veto: %s — %s (%s)\n", formatDuration(qVetoGap), qVetoUser, qVetoBeer)
	}
	if hasSVeto && (sVetoGap != qVetoGap || sVetoUser != qVetoUser) {
		fmt.Fprintf(&b, "Slowest veto: %s — %s (%s)\n", formatDuration(sVetoGap), sVetoUser, sVetoBeer)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

type firstDrink struct {
	user string
	at   time.Time
}

// firstToDrink returns all users ranked by their earliest consumed event,
// each user listed once.
func firstToDrink(events []ConsumedEvent) []firstDrink {
	earliest := map[string]time.Time{}
	for _, e := range events {
		if e.ConsumedByName == "" {
			continue
		}
		if t, ok := earliest[e.ConsumedByName]; !ok || e.ConsumedAt.Before(t) {
			earliest[e.ConsumedByName] = e.ConsumedAt
		}
	}
	out := make([]firstDrink, 0, len(earliest))
	for u, t := range earliest {
		out = append(out, firstDrink{u, t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

func userGapExtremes(events []ConsumedEvent) (fastUser string, fastGap time.Duration, hasFast bool,
	slowUser string, slowGap time.Duration, hasSlow bool) {
	byUser := map[string][]time.Time{}
	for _, e := range events {
		name := e.ConsumedByName
		if name == "" {
			continue
		}
		byUser[name] = append(byUser[name], e.ConsumedAt)
	}
	for user, times := range byUser {
		if len(times) < 2 {
			continue
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		for i := 1; i < len(times); i++ {
			gap := times[i].Sub(times[i-1])
			if !hasFast || gap < fastGap {
				fastGap, fastUser, hasFast = gap, user, true
			}
			if !hasSlow || gap > slowGap {
				slowGap, slowUser, hasSlow = gap, user, true
			}
		}
	}
	return
}

// powerHour finds the 60-minute sliding window containing the most events.
// Returns the timestamp of the first event in that window, the count, and
// ok=false when fewer than 2 events ever fall within a 60-min span.
func powerHour(events []ConsumedEvent) (start time.Time, count int, ok bool) {
	if len(events) < 2 {
		return
	}
	times := make([]time.Time, len(events))
	for i, e := range events {
		times[i] = e.ConsumedAt
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	bestLeft, bestCount, left := 0, 0, 0
	for right := range times {
		for times[right].Sub(times[left]) > time.Hour {
			left++
		}
		if n := right - left + 1; n > bestCount {
			bestCount, bestLeft = n, left
		}
	}
	if bestCount < 2 {
		return
	}
	return times[bestLeft], bestCount, true
}

type hourStreak struct {
	user      string
	hours     int
	startHour time.Time
	endHour   time.Time
}

// longestHourStreak returns every user tied for the longest run of consecutive
// clock hours each containing at least one beer. Returns nil when no user has
// a streak of at least 2 hours. Each user appears at most once (their
// earliest-ending best run). Result is sorted by endHour ascending, with
// alphabetical tiebreak.
func longestHourStreak(events []ConsumedEvent, loc *time.Location) []hourStreak {
	byUser := map[string]map[time.Time]struct{}{}
	for _, e := range events {
		u := e.ConsumedByName
		if u == "" {
			continue
		}
		t := e.ConsumedAt.In(loc)
		bucket := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
		set, ok := byUser[u]
		if !ok {
			set = map[time.Time]struct{}{}
			byUser[u] = set
		}
		set[bucket] = struct{}{}
	}

	perUser := make([]hourStreak, 0, len(byUser))
	for user, set := range byUser {
		hours := make([]time.Time, 0, len(set))
		for h := range set {
			hours = append(hours, h)
		}
		sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })

		var best hourStreak
		haveBest := false
		consider := func(s hourStreak) {
			if !haveBest || s.hours > best.hours || (s.hours == best.hours && s.endHour.Before(best.endHour)) {
				best, haveBest = s, true
			}
		}
		runStart, runLen := hours[0], 1
		for i := 1; i < len(hours); i++ {
			if hours[i].Sub(hours[i-1]) == time.Hour {
				runLen++
				continue
			}
			consider(hourStreak{user, runLen, runStart, hours[i-1]})
			runStart, runLen = hours[i], 1
		}
		consider(hourStreak{user, runLen, runStart, hours[len(hours)-1]})

		if best.hours >= 2 {
			perUser = append(perUser, best)
		}
	}

	if len(perUser) == 0 {
		return nil
	}
	sort.Slice(perUser, func(i, j int) bool {
		if perUser[i].hours != perUser[j].hours {
			return perUser[i].hours > perUser[j].hours
		}
		if !perUser[i].endHour.Equal(perUser[j].endHour) {
			return perUser[i].endHour.Before(perUser[j].endHour)
		}
		return perUser[i].user < perUser[j].user
	})
	return perUser
}

type beerStreak struct {
	user  string
	count int
	start time.Time
	end   time.Time
}

// longestBeerStreak returns every user tied for the longest run of consecutive
// beers where each consecutive pair is within gap of each other. Returns nil
// when no user has a streak of at least 2 beers.
func longestBeerStreak(events []ConsumedEvent, gap time.Duration) []beerStreak {
	byUser := map[string][]time.Time{}
	for _, e := range events {
		if e.ConsumedByName == "" {
			continue
		}
		byUser[e.ConsumedByName] = append(byUser[e.ConsumedByName], e.ConsumedAt)
	}

	perUser := make([]beerStreak, 0, len(byUser))
	for user, times := range byUser {
		if len(times) < 2 {
			continue
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

		var best beerStreak
		haveBest := false
		consider := func(s beerStreak) {
			if !haveBest || s.count > best.count || (s.count == best.count && s.end.Before(best.end)) {
				best, haveBest = s, true
			}
		}
		runStart, runCount := times[0], 1
		for i := 1; i < len(times); i++ {
			if times[i].Sub(times[i-1]) <= gap {
				runCount++
				continue
			}
			consider(beerStreak{user, runCount, runStart, times[i-1]})
			runStart, runCount = times[i], 1
		}
		consider(beerStreak{user, runCount, runStart, times[len(times)-1]})

		if best.count >= 2 {
			perUser = append(perUser, best)
		}
	}

	if len(perUser) == 0 {
		return nil
	}
	sort.Slice(perUser, func(i, j int) bool {
		if perUser[i].count != perUser[j].count {
			return perUser[i].count > perUser[j].count
		}
		if !perUser[i].end.Equal(perUser[j].end) {
			return perUser[i].end.Before(perUser[j].end)
		}
		return perUser[i].user < perUser[j].user
	})
	return perUser
}

// vetoDecisionExtremes returns the shortest and longest decision durations
// reported by the API for vetoed events.
func vetoDecisionExtremes(vetoes []VetoedEvent) (
	fastUser, fastBeer string, fastGap time.Duration, hasFast bool,
	slowUser, slowBeer string, slowGap time.Duration, hasSlow bool,
) {
	for _, v := range vetoes {
		if v.VetoedByName == "" || v.DecisionSeconds <= 0 {
			continue
		}
		g := time.Duration(v.DecisionSeconds * float64(time.Second))
		beer := beerLabel(v.ProductNameBold, v.ProductNameThin)
		if !hasFast || g < fastGap {
			fastGap, fastUser, fastBeer, hasFast = g, v.VetoedByName, beer, true
		}
		if !hasSlow || g > slowGap {
			slowGap, slowUser, slowBeer, hasSlow = g, v.VetoedByName, beer, true
		}
	}
	return
}

// acceptDecisionExtremes returns the shortest and longest decision durations
// reported by the API for consumed events.
func acceptDecisionExtremes(events []ConsumedEvent) (
	fastUser, fastBeer string, fastGap time.Duration, hasFast bool,
	slowUser, slowBeer string, slowGap time.Duration, hasSlow bool,
) {
	for _, e := range events {
		if e.ConsumedByName == "" || e.DecisionSeconds <= 0 {
			continue
		}
		g := time.Duration(e.DecisionSeconds * float64(time.Second))
		beer := beerLabel(e.ProductNameBold, e.ProductNameThin)
		if !hasFast || g < fastGap {
			fastGap, fastUser, fastBeer, hasFast = g, e.ConsumedByName, beer, true
		}
		if !hasSlow || g > slowGap {
			slowGap, slowUser, slowBeer, hasSlow = g, e.ConsumedByName, beer, true
		}
	}
	return
}

func beerLabel(bold, thin string) string {
	label := sanitize(bold)
	if label == "" {
		label = "?"
	}
	if thin != "" {
		label += " — " + sanitize(thin)
	}
	return label
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// topStrongest returns up to n events ranked by AlcoholPercent (desc),
// ties broken by earlier ConsumedAt. Events with AlcoholPercent <= 0 are skipped.
func topStrongest(events []ConsumedEvent, n int) []ConsumedEvent {
	ranked := make([]ConsumedEvent, 0, len(events))
	for _, e := range events {
		if e.AlcoholPercent > 0 {
			ranked = append(ranked, e)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].AlcoholPercent != ranked[j].AlcoholPercent {
			return ranked[i].AlcoholPercent > ranked[j].AlcoholPercent
		}
		return ranked[i].ConsumedAt.Before(ranked[j].ConsumedAt)
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	return ranked
}

type vetoDetail struct {
	user string
	beer string
	at   time.Time
}

// vetoDetails returns each veto's user, vetoed-product name, and veto time,
// ordered chronologically. Vetoes with an empty VetoedByName are skipped.
func vetoDetails(vetoes []VetoedEvent) []vetoDetail {
	vs := make([]VetoedEvent, 0, len(vetoes))
	for _, v := range vetoes {
		if v.VetoedByName == "" {
			continue
		}
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool {
		return vs[i].VetoedAt.Before(vs[j].VetoedAt)
	})

	out := make([]vetoDetail, 0, len(vs))
	for _, v := range vs {
		beer := sanitize(v.ProductNameBold)
		if beer == "" {
			beer = "?"
		}
		if v.ProductNameThin != "" {
			beer += " — " + sanitize(v.ProductNameThin)
		}
		out = append(out, vetoDetail{v.VetoedByName, beer, v.VetoedAt})
	}
	return out
}

type vetoUser struct {
	name  string
	count int
}

// vetoStatus partitions participants into those who have used at least one veto
// (descending count, alphabetical tiebreak) and those who have not (alphabetical).
func vetoStatus(participants []Participant, vetoes []VetoedEvent) (used []vetoUser, notUsed []string) {
	counts := map[string]int{}
	for _, v := range vetoes {
		name := v.VetoedByName
		if name == "" {
			continue
		}
		counts[name]++
	}
	for _, p := range participants {
		if p.Username == "" {
			continue
		}
		if c, ok := counts[p.Username]; ok {
			used = append(used, vetoUser{p.Username, c})
		} else {
			notUsed = append(notUsed, p.Username)
		}
	}
	sort.Slice(used, func(i, j int) bool {
		if used[i].count != used[j].count {
			return used[i].count > used[j].count
		}
		return used[i].name < used[j].name
	})
	sort.Strings(notUsed)
	return used, notUsed
}

// measureBodyHeight renders body text onto a canvas large enough not to clip,
// using the same font/pointsize/offset as generatePNG, and returns the y-bottom
// of the ink bounding box. This catches overflow that a pure line-count estimate
// misses (font fallback glyphs render taller than plain Courier).
func measureBodyHeight(body string) (int, error) {
	cmd := exec.Command("convert",
		"-size", fmt.Sprintf("%dx4000", bodyWidth), "xc:white",
		"-font", "Courier",
		"-pointsize", strconv.Itoa(bodyPointSize),
		"-fill", "black",
		"-gravity", "NorthWest",
		"-annotate", "+20+20", body,
		"-format", "%@",
		"info:",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%w (output: %s)", err, string(out))
	}
	s := strings.TrimSpace(string(out))
	var w, h, x, y int
	if _, err := fmt.Sscanf(s, "%dx%d+%d+%d", &w, &h, &x, &y); err != nil {
		return 0, fmt.Errorf("parse bbox %q: %w", s, err)
	}
	_ = w
	_ = x
	return y + h, nil
}

// splitToFitHeight ensures each returned page renders within maxHeight pixels.
// If a page overflows, it walks back to the latest blank line that produces a
// fitting first part and recurses on the remainder.
func splitToFitHeight(page string, maxHeight int) []string {
	h, err := measureBodyHeight(page)
	if err != nil {
		log.Printf("measure: %v", err)
		return []string{page}
	}
	if h <= maxHeight {
		return []string{page}
	}
	lines := strings.Split(page, "\n")
	for cut := len(lines) - 1; cut > 0; cut-- {
		if strings.TrimSpace(lines[cut]) != "" {
			continue
		}
		first := strings.Join(lines[:cut], "\n")
		h1, err := measureBodyHeight(first)
		if err != nil || h1 > maxHeight {
			continue
		}
		rest := strings.Join(lines[cut+1:], "\n")
		return append([]string{first}, splitToFitHeight(rest, maxHeight)...)
	}
	log.Printf("page too tall (%dpx > %dpx) and could not be split at a blank line", h, maxHeight)
	return []string{page}
}

func generatePNG(path, body string) error {
	cmd := exec.Command("convert",
		"-size", fmt.Sprintf("%dx%d", bodyWidth, bodyHeight), "xc:white",
		"-font", "Courier",
		"-pointsize", strconv.Itoa(bodyPointSize),
		"-fill", "black",
		"-gravity", "NorthWest",
		"-annotate", "+20+20", body,
		path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, string(out))
	}
	return nil
}

// paginate splits body text into pages of at most linesPerPage lines each,
// preferring to break at blank-line section boundaries so a section is not
// split across pages. Always returns at least one page.
func paginate(body string, linesPerPage int) []string {
	lines := strings.Split(body, "\n")
	pages := []string{}
	i := 0
	for i < len(lines) {
		end := min(i+linesPerPage, len(lines))
		if end < len(lines) {
			// Walk back to the last blank line inside this window so we
			// break between sections instead of mid-section.
			for j := end - 1; j > i; j-- {
				if strings.TrimSpace(lines[j]) == "" {
					end = j
					break
				}
			}
		}
		pages = append(pages, strings.Join(lines[i:end], "\n"))
		// Consume the boundary blank line so it doesn't lead the next page.
		if end < len(lines) && strings.TrimSpace(lines[end]) == "" {
			i = end + 1
		} else {
			i = end
		}
	}
	if len(pages) == 0 {
		pages = []string{""}
	}
	return pages
}

// combineTIFFs merges single-page Fax TIFFs into one multi-page Fax TIFF.
func combineTIFFs(inputs []string, output string) error {
	args := append([]string{}, inputs...)
	args = append(args, "-compress", "Fax", "-type", "bilevel", output)
	cmd := exec.Command("convert", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, string(out))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func barChars(value, max, width int) string {
	if max <= 0 || value <= 0 {
		return ""
	}
	n := min(value*width/max, width)
	return strings.Repeat("█", n)
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
