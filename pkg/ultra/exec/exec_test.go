package exec

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeEnv makes the test binary act as a fake agent CLI instead of tests.
const fakeEnv = "PAI_ULTRA_FAKE_CLI"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeEnv) {
	case "claude":
		fakeClaude()
	case "codex":
		fakeCodex()
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

const claim = "```pai-result\n{\"status\":\"done\",\"summary\":\"fixed it\",\"changes\":[\"a.go\"],\"evidence\":[\"go test ok\"]}\n```"

func fakeClaude() {
	prompt, _ := io.ReadAll(os.Stdin)
	resume := ""
	for i, a := range os.Args {
		if a == "--resume" && i+1 < len(os.Args) {
			resume = os.Args[i+1]
		}
	}
	sess := "sess-new"
	if resume != "" {
		sess = resume
	}
	fmt.Printf(`{"type":"system","subtype":"init","session_id":%q}`+"\n", sess)
	fmt.Println(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"},{"type":"text","text":"working"}]},"session_id":"` + sess + `"}`)
	result := "saw " + fmt.Sprint(len(prompt)) + " bytes\n" + claim
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"result":%q,"usage":{"input_tokens":10,"cache_creation_input_tokens":5,"cache_read_input_tokens":100,"output_tokens":7}}`+"\n", sess, result)
	os.Exit(0)
}

func fakeCodex() {
	io.ReadAll(os.Stdin)
	fmt.Println(`{"type":"thread.started","thread_id":"thread-1"}`)
	fmt.Println(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}`)
	fmt.Printf(`{"type":"item.completed","item":{"type":"agent_message","text":%q}}`+"\n", "done\n"+claim)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":20}}`)
	os.Exit(0)
}

func drain(r Run) []Event {
	var evs []Event
	for e := range r.Events() {
		evs = append(evs, e)
	}
	return evs
}

func TestClaudeAdapterParsesStream(t *testing.T) {
	t.Setenv(fakeEnv, "claude")
	run, err := Claude{Bin: os.Args[0]}.Start(context.Background(), Assignment{TaskID: "t1", Workdir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(run)
	res := run.Wait()
	if res.Status != "done" || res.Summary != "fixed it" || res.NativeSession != "sess-new" || !res.Usage.Reported || res.Usage.Total() != 122 {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(res.Final, "saw 5 bytes") {
		t.Fatal("prompt not sent on stdin")
	}
	tools := 0
	for _, e := range evs {
		if e.Kind == "tool" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("events: %+v", evs)
	}
	// Resume passes the native session.
	run, _ = Claude{Bin: os.Args[0]}.Start(context.Background(), Assignment{TaskID: "t1", Workdir: t.TempDir(), Prompt: "again", Resume: "sess-old"})
	drain(run)
	if r := run.Wait(); r.NativeSession != "sess-old" {
		t.Fatalf("resume: %+v", r)
	}
}

func TestCodexAdapterParsesStream(t *testing.T) {
	t.Setenv(fakeEnv, "codex")
	run, err := Codex{Bin: os.Args[0]}.Start(context.Background(), Assignment{TaskID: "t1", Workdir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	drain(run)
	res := run.Wait()
	if res.Status != "done" || res.NativeSession != "thread-1" || res.Usage.CacheRead != 800 || res.Usage.Input != 200 || res.Usage.Output != 20 {
		t.Fatalf("result: %+v", res)
	}
}

func TestCancelStopsTheProcess(t *testing.T) {
	t.Setenv(fakeEnv, "hang")
	run, err := Claude{Bin: os.Args[0]}.Start(context.Background(), Assignment{TaskID: "t1", Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	run.Cancel()
	drain(run)
	res := run.Wait()
	if res.Status != "cancelled" || time.Since(start) > 15*time.Second {
		t.Fatalf("cancel: %+v after %s", res, time.Since(start))
	}
}

func TestTimeoutFails(t *testing.T) {
	t.Setenv(fakeEnv, "hang")
	run, _ := Codex{Bin: os.Args[0]}.Start(context.Background(), Assignment{TaskID: "t1", Workdir: t.TempDir(), Timeout: 300 * time.Millisecond})
	drain(run)
	if res := run.Wait(); res.Status != "failed" || !strings.Contains(res.Err, "timed out") {
		t.Fatalf("timeout: %+v", res)
	}
}

func TestParseResult(t *testing.T) {
	if r := ParseResult("no block"); r.Status != "unknown" {
		t.Fatal("missing block is not unknown")
	}
	if r := ParseResult("```pai-result\n{\"status\":\"accepted\"}\n```"); r.Status != "unknown" {
		t.Fatal("an executor may not claim a coordinator status")
	}
	two := "```pai-result\n{\"status\":\"failed\"}\n```\nlater\n```pai-result\n{\"status\":\"done\"}\n```"
	if r := ParseResult(two); r.Status != "done" {
		t.Fatal("last block not used")
	}
}

func TestUnavailableCLI(t *testing.T) {
	if err := (Claude{Bin: "definitely-not-installed-pai"}).Available(context.Background()); err == nil {
		t.Fatal("missing CLI reported available")
	}
	if err := (PAI{}).Available(context.Background()); err == nil {
		t.Fatal("PAI adapter without a server reported available")
	}
}
