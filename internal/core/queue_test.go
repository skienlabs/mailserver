package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRemote struct {
	mu        sync.Mutex
	active    map[string]int
	maxDomain map[string]int
	total     int
	maxTotal  int
	delivered map[string]int
	delay     func(domain string) time.Duration
	fail      func(rcpt string) error
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{active: map[string]int{}, maxDomain: map[string]int{}, delivered: map[string]int{}}
}

func (f *fakeRemote) send(ctx context.Context, domain, from string, rcpts []string, raw []byte) (map[string]error, string) {
	f.mu.Lock()
	f.active[domain]++
	f.total++
	f.maxDomain[domain] = max(f.maxDomain[domain], f.active[domain])
	f.maxTotal = max(f.maxTotal, f.total)
	f.mu.Unlock()
	d := 10 * time.Millisecond
	if f.delay != nil {
		d = f.delay(domain)
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
	res := map[string]error{}
	f.mu.Lock()
	for _, r := range rcpts {
		if f.fail != nil {
			if err := f.fail(r); err != nil {
				res[r] = err
				continue
			}
		}
		if ctx.Err() != nil {
			res[r] = tempErr("interrupted")
			continue
		}
		res[r] = nil
		f.delivered[r]++
	}
	f.active[domain]--
	f.total--
	f.mu.Unlock()
	return res, "mx." + domain
}

func queueApp(t *testing.T) (*App, *User, *fakeRemote) {
	t.Helper()
	app, _, a, _ := newTestApp(t, false)
	f := newFakeRemote()
	app.qd.send = f.send
	app.Go(app.queueWorker)
	return app, a, f
}

func submitRemote(t *testing.T, app *App, u *User, rcpts []string, subject string) {
	t.Helper()
	raw := []byte(fmt.Sprintf("From: asi@skien.test\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s@skien.test>\r\n\r\nhello\r\n", strings.Join(rcpts, ", "), subject, RandomHex(8)))
	if err := app.Submit(SubmitReq{User: u, EnvFrom: u.Email, Raw: raw, Rcpts: rcpts, Via: "web"}); err != nil {
		t.Fatal(err)
	}
}

func waitQueue(t *testing.T, app *App, cond string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var n int
	for time.Now().Before(deadline) {
		app.DB.QueryRow(`SELECT COUNT(*) FROM queue_rcpts WHERE ` + cond).Scan(&n)
		if n == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("queue: %d rows match %q, want %d", n, cond, want)
}

func TestQueueThroughputAndLimits(t *testing.T) {
	app, a, f := queueApp(t)
	// 600 recipients: 30 messages × 20 recipients spread over 5 domains.
	for m := 0; m < 30; m++ {
		var rcpts []string
		for i := 0; i < 20; i++ {
			rcpts = append(rcpts, fmt.Sprintf("u%d.%d@dest%d.test", m, i, i%5))
		}
		submitRemote(t, app, a, rcpts, fmt.Sprintf("bulk %d", m))
	}
	start := time.Now()
	waitQueue(t, app, "status='sent'", 600, 30*time.Second)
	t.Logf("600 recipients delivered in %v", time.Since(start).Round(time.Millisecond))
	f.mu.Lock()
	defer f.mu.Unlock()
	for d, m := range f.maxDomain {
		if m > app.qd.perDomain {
			t.Fatalf("%d parallel sessions to %s (limit %d)", m, d, app.qd.perDomain)
		}
	}
	if f.maxTotal > app.qd.workers {
		t.Fatalf("%d parallel sessions (workers %d)", f.maxTotal, app.qd.workers)
	}
	if f.maxTotal < 5 {
		t.Fatalf("only %d sessions ran in parallel", f.maxTotal)
	}
	for r, n := range f.delivered {
		if n != 1 {
			t.Fatalf("%s delivered %d times", r, n)
		}
	}
	var logged int
	app.DB.QueryRow(`SELECT COUNT(*) FROM queue_attempts WHERE result='sent'`).Scan(&logged)
	if logged != 600 {
		t.Fatalf("%d attempts logged", logged)
	}
}

func TestQueueSlowDestinationDoesNotBlockOthers(t *testing.T) {
	app, a, f := queueApp(t)
	f.delay = func(domain string) time.Duration {
		if domain == "slow.test" {
			return 3 * time.Second
		}
		return 5 * time.Millisecond
	}
	for i := 0; i < 6; i++ {
		submitRemote(t, app, a, []string{fmt.Sprintf("x%d@slow.test", i)}, "to slow")
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 40; i++ {
		submitRemote(t, app, a, []string{fmt.Sprintf("y%d@fast%d.test", i, i%4)}, "to fast")
	}
	waitQueue(t, app, "status='sent' AND domain LIKE 'fast%'", 40, 2*time.Second) // while slow.test is still busy
	var slowDone int
	app.DB.QueryRow(`SELECT COUNT(*) FROM queue_rcpts WHERE status='sent' AND domain='slow.test'`).Scan(&slowDone)
	if slowDone != 0 {
		t.Fatalf("slow destination finished too early (%d)", slowDone)
	}
	waitQueue(t, app, "status='sent' AND domain='slow.test'", 6, 15*time.Second)
}

func TestQueueRetriesAndAdminViews(t *testing.T) {
	app, a, f := queueApp(t)
	f.fail = func(rcpt string) error {
		switch {
		case strings.HasPrefix(rcpt, "busy"):
			return tempErr("mx.busy.test said: 451 try later")
		case strings.HasPrefix(rcpt, "nouser"):
			return &deliveryError{perm: true, msg: "mx.dest.test said: 550 no such user"}
		}
		return nil
	}
	submitRemote(t, app, a, []string{"ok@dest.test", "busy@dest.test", "nouser@dest.test"}, "mixed")
	waitQueue(t, app, "status IN ('sent','failed','deferred') AND attempts=1", 3, 5*time.Second)
	var st, lastErr string
	app.DB.QueryRow(`SELECT status, last_error FROM queue_rcpts WHERE rcpt='busy@dest.test'`).Scan(&st, &lastErr)
	if st != "deferred" || !strings.Contains(lastErr, "451") {
		t.Fatalf("busy: %s %q", st, lastErr)
	}
	// the sender got a bounce for the permanent failure
	waitFor(t, "bounce in inbox", func() bool {
		var n int
		app.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE user_id=? AND subject LIKE 'Delivery Status Notification%'`, a.ID).Scan(&n)
		return n == 1
	})

	call := func(method, path string, h func(http.ResponseWriter, *http.Request, *User), pv map[string]string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		for k, v := range pv {
			r.SetPathValue(k, v)
		}
		h(w, r, a)
		return w
	}
	w := call("GET", "/api/admin/queue?view=retrying", app.handleAdminQueue, nil)
	if !strings.Contains(w.Body.String(), "busy@dest.test") || strings.Contains(w.Body.String(), `"rcpt":"ok@dest.test"`) {
		t.Fatalf("retrying view: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `"retrying":1`) || !strings.Contains(w.Body.String(), `"sent_24h":1`) || !strings.Contains(w.Body.String(), `"failed_24h":1`) {
		t.Fatalf("summary: %s", w.Body)
	}
	w = call("GET", "/api/admin/queue?view=all&q=nouser", app.handleAdminQueue, nil)
	if !strings.Contains(w.Body.String(), "nouser@dest.test") || strings.Contains(w.Body.String(), `"rcpt":"busy@dest.test"`) {
		t.Fatalf("search: %s", w.Body)
	}
	var qid int64
	app.DB.QueryRow(`SELECT queue_id FROM queue_rcpts WHERE rcpt='busy@dest.test'`).Scan(&qid)
	w = call("GET", "/x", app.handleAdminQueueMessage, map[string]string{"id": fmt.Sprint(qid)})
	body := w.Body.String()
	if !strings.Contains(body, `"subject":"mixed"`) || !strings.Contains(body, "Subject: mixed") || !strings.Contains(body, "451 try later") || !strings.Contains(body, `"host":"mx.dest.test"`) {
		t.Fatalf("message detail: %s", body)
	}
	// Retry all: the deferred recipient is tried again right away.
	f.fail = nil
	call("POST", "/x", app.handleAdminQueueRetryAll, nil)
	waitQueue(t, app, "status='sent'", 2, 5*time.Second)
	var attempts int
	app.DB.QueryRow(`SELECT COUNT(*) FROM queue_attempts WHERE rcpt='busy@dest.test'`).Scan(&attempts)
	if attempts != 2 {
		t.Fatalf("%d attempts logged for busy@", attempts)
	}
}

func TestQueueCancelAndShutdown(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	f := newFakeRemote()
	f.delay = func(string) time.Duration { return time.Hour } // never finishes on its own
	app.qd.send = f.send
	app.Go(app.queueWorker)
	submitRemote(t, app, a, []string{"stuck@dest.test"}, "stuck")
	waitFor(t, "delivery started", func() bool { return app.QueueActive() == 1 })
	// Shutting down mid-delivery leaves it queued, with no attempt counted.
	app.cancel()
	app.wg.Wait()
	var st string
	var attempts int
	app.DB.QueryRow(`SELECT status, attempts FROM queue_rcpts WHERE rcpt='stuck@dest.test'`).Scan(&st, &attempts)
	if st != "pending" || attempts != 0 {
		t.Fatalf("after shutdown: %s, %d attempts", st, attempts)
	}
	// An admin can stop a waiting recipient; it's logged.
	var id int64
	app.DB.QueryRow(`SELECT id FROM queue_rcpts WHERE rcpt='stuck@dest.test'`).Scan(&id)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", nil)
	r.SetPathValue("id", fmt.Sprint(id))
	app.handleAdminQueueCancel(w, r, a)
	app.DB.QueryRow(`SELECT status FROM queue_rcpts WHERE id=?`, id).Scan(&st)
	var logged string
	app.DB.QueryRow(`SELECT detail FROM queue_attempts WHERE rcpt_id=?`, id).Scan(&logged)
	if st != "failed" || !strings.Contains(logged, "Stopped by asi@skien.test") {
		t.Fatalf("cancel: %s %q", st, logged)
	}
}

// A recipient that looks due in a stale scan but was already delivered (or stopped) by the time
// a worker picks it up is never sent again.
func TestQueueStaleScanNeverResends(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	f := newFakeRemote()
	app.qd.send = f.send
	submitRemote(t, app, a, []string{"once@dest.test", "stopped@dest.test", "later@dest.test", "due@dest.test"}, "stale")
	var list []dueRcpt
	rows, err := app.DB.Query(`SELECT id, queue_id, rcpt, domain, attempts FROM queue_rcpts ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d dueRcpt
		rows.Scan(&d.id, &d.queueID, &d.rcpt, &d.domain, &d.attempts)
		list = append(list, d)
	}
	rows.Close()
	// Meanwhile: one was delivered, one stopped by an admin, one deferred into the future.
	app.finishRcpt(list[0], "sent", "")
	app.finishRcpt(list[1], "failed", "Stopped by admin")
	app.DB.Exec(`UPDATE queue_rcpts SET status='deferred', next_attempt=? WHERE id=?`, nowMs()+3600_000, list[2].id)
	app.deliverGroup(t.Context(), list)
	if len(f.delivered) != 1 || f.delivered["due@dest.test"] != 1 {
		t.Fatalf("delivered: %v", f.delivered)
	}
}

// Shutdown during a session where the server accepted one recipient and refused another for
// good: both outcomes are recorded, so the accepted one isn't sent again after the restart.
func TestQueueShutdownKeepsFinalResults(t *testing.T) {
	app, _, a, _ := newTestApp(t, false)
	submitRemote(t, app, a, []string{"ok@dest.test", "nouser@dest.test", "later@dest.test"}, "mixed")
	ctx, cancel := context.WithCancel(t.Context())
	sends := map[string]int{}
	app.qd.send = func(_ context.Context, domain, from string, rcpts []string, raw []byte) (map[string]error, string) {
		res := map[string]error{}
		for _, r := range rcpts {
			sends[r]++
			switch r {
			case "nouser@dest.test":
				res[r] = &deliveryError{perm: true, msg: "550 no such user"}
			case "later@dest.test":
				res[r] = tempErr("interrupted")
			default:
				res[r] = nil
			}
		}
		cancel() // SIGTERM arrives as the session ends
		return res, "mx.dest.test"
	}
	var list []dueRcpt
	rows, _ := app.DB.Query(`SELECT id, queue_id, rcpt, domain, attempts FROM queue_rcpts ORDER BY id`)
	for rows.Next() {
		var d dueRcpt
		rows.Scan(&d.id, &d.queueID, &d.rcpt, &d.domain, &d.attempts)
		list = append(list, d)
	}
	rows.Close()
	app.deliverGroup(ctx, list)
	status := func(r string) (st string, attempts int) {
		app.DB.QueryRow(`SELECT status, attempts FROM queue_rcpts WHERE rcpt=?`, r).Scan(&st, &attempts)
		return
	}
	if st, _ := status("ok@dest.test"); st != "sent" {
		t.Fatalf("accepted recipient: %s", st)
	}
	if st, _ := status("nouser@dest.test"); st != "failed" {
		t.Fatalf("refused recipient: %s", st)
	}
	if st, n := status("later@dest.test"); st != "pending" || n != 0 {
		t.Fatalf("unfinished recipient: %s, %d attempts", st, n)
	}
	app.qd.send = func(_ context.Context, domain, from string, rcpts []string, raw []byte) (map[string]error, string) {
		res := map[string]error{}
		for _, r := range rcpts {
			sends[r]++
			res[r] = nil
		}
		return res, "mx.dest.test"
	}
	app.deliverGroup(t.Context(), list) // the next start
	if sends["ok@dest.test"] != 1 || sends["nouser@dest.test"] != 1 || sends["later@dest.test"] != 2 {
		t.Fatalf("sends: %v", sends)
	}
}
