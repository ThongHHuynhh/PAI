import { useEffect, useRef, useState } from "react";
import type { Rec } from "../api";

// Speech is the browser's own. Safari and Chrome can listen, Firefox cannot,
// and there the voice buttons are not shown.
const Recognition: any = (window as any).SpeechRecognition ?? (window as any).webkitSpeechRecognition;
export const canListen = !!Recognition;
// READ is where this browser keeps whether replies are read aloud.
export const READ = "pai.voice.read";
// SILENCE is how long voice mode waits after the last word before it sends.
const SILENCE = 1500;

// wake returns what follows "PAI" or "Hey PAI" at the start of t, as a
// recognizer may hear it ("pie", "hey pay"), or null when t has no wake phrase.
export function wake(t: string): string | null {
  const m = t.match(/^\W*(?:(?:hey|hi|ok|okay)\W+)?(?:p\W?a\W?i|pie|pi|pay|pye)\b\W*(.*)$/is);
  return m ? m[1] : null;
}

function listener() {
  const r = new Recognition();
  r.continuous = true;
  r.interimResults = true;
  r.lang = navigator.language;
  return r;
}

// useDictation writes speech into the message box: each final phrase goes to
// add, and the phrase still being heard is shown as interim.
export function useDictation(add: (t: string) => void) {
  const [interim, setInterim] = useState<string | null>(null); // null when not listening
  const rec = useRef<any>(null);
  useEffect(() => () => rec.current?.abort(), []);
  const toggle = () => {
    if (rec.current) return rec.current.stop();
    const r = listener();
    r.onresult = (e: any) => {
      let now = "";
      for (let i = e.resultIndex; i < e.results.length; i++) {
        if (e.results[i].isFinal) add(e.results[i][0].transcript.trim());
        else now += e.results[i][0].transcript;
      }
      setInterim(now.trim());
    };
    r.onend = () => {
      rec.current = null;
      setInterim(null);
    };
    rec.current = r;
    setInterim("");
    r.start();
  };
  return { on: interim !== null, interim: interim ?? "", toggle };
}

export type VoiceState = "off" | "wake" | "hearing" | "sending" | "speaking";

// useVoiceMode listens while it is on for the wake phrase, then for the
// message after it, and sends the message after a short silence. When
// Settings say so it reads the session's replies aloud, and does not listen
// meanwhile, so it does not hear itself. A recognizer stops by itself after
// a silence or an error: it starts again, later after each error in a row.
export function useVoiceMode(send: (t: string) => Promise<void>, recs: Rec[], working: boolean, fail: (e: string) => void) {
  const [state, setState] = useState<VoiceState>("off");
  const [heard, setHeard] = useState("");
  const out = useRef({ send, fail });
  out.current = { send, fail };
  // from is the result the message starts at in this recognizer, -1 while
  // waiting for the wake phrase; woke says that result begins with it.
  // before is the message heard by recognizers that already ended.
  const v = useRef({ on: false, busy: false, rec: null as any, errs: 0, timer: 0, from: -1, woke: false, before: "", spoken: 0 }).current;

  const reset = () => {
    window.clearTimeout(v.timer);
    v.from = -1;
    v.before = "";
    setHeard("");
  };
  const listen = () => {
    if (!v.on || v.busy || v.rec) return;
    const r = listener();
    let msg = v.before;
    if (v.from >= 0) {
      v.from = 0;
      v.woke = false;
    }
    r.onresult = (e: any) => {
      v.errs = 0;
      const said: string[] = [...e.results].map((x: any) => x[0].transcript);
      if (v.from < 0) {
        v.from = said.findIndex((t) => wake(t) !== null);
        v.woke = true;
        if (v.from < 0) return;
      }
      const [first, ...rest] = said.slice(v.from);
      const head = v.woke ? wake(first ?? "") : first;
      if (head === null) {
        // what sounded like the wake phrase was not
        reset();
        setState("wake");
        return;
      }
      msg = [v.before, head, ...rest].join(" ").trim();
      setState("hearing");
      setHeard(msg);
      window.clearTimeout(v.timer);
      v.timer = window.setTimeout(() => (msg ? void post(msg) : (reset(), setState("wake"))), msg ? SILENCE : 8000);
    };
    r.onerror = (e: any) => {
      if (e.error === "not-allowed" || e.error === "service-not-allowed") {
        off();
        out.current.fail("Voice mode needs the microphone. Allow it for this site and turn voice mode on again.");
      } else if (e.error !== "no-speech" && e.error !== "aborted") v.errs++;
    };
    r.onend = () => {
      if (v.rec === r) v.rec = null;
      if (v.from >= 0) v.before = msg;
      if (v.on && !v.busy) window.setTimeout(listen, v.errs ? Math.min(30000, 1000 * 2 ** v.errs) : 300);
    };
    v.rec = r;
    r.start();
  };
  // pause stops listening for what busy does, then listens again
  const pause = async (s: VoiceState, busy: () => Promise<void>) => {
    v.busy = true;
    reset();
    setState(s);
    v.rec?.abort();
    await busy();
    v.busy = false;
    if (!v.on) return;
    setState("wake");
    listen();
  };
  const post = (msg: string) => pause("sending", () => out.current.send(msg).catch((e: any) => out.current.fail(String(e.message ?? e))));
  const off = () => {
    v.on = false;
    reset();
    v.rec?.abort();
    speechSynthesis.cancel();
    setState("off");
  };
  const toggle = () => {
    if (v.on) return off();
    v.on = true;
    v.busy = false;
    v.errs = 0;
    v.spoken = recs.length ? recs[recs.length - 1].n : 0;
    speechSynthesis.speak(new SpeechSynthesisUtterance("")); // iOS speaks later only if the page spoke in a tap
    setState("wake");
    listen();
  };

  // a hidden page cannot listen: voice mode ends with it
  useEffect(() => {
    const hide = () => document.hidden && v.on && off();
    document.addEventListener("visibilitychange", hide);
    return () => {
      document.removeEventListener("visibilitychange", hide);
      if (v.on) off();
    };
  }, []);

  // the last text of a finished turn is the reply; tool calls are not read
  useEffect(() => {
    const last = [...recs].reverse().find((r) => r.kind === "text");
    if (!v.on || working || !last || last.n <= v.spoken) return;
    v.spoken = last.n;
    if (localStorage.getItem(READ) !== "1") return;
    void pause("speaking", () =>
      new Promise((done) => {
        const u = new SpeechSynthesisUtterance(last.body.replace(/```[\s\S]*?```/g, " "));
        u.onend = u.onerror = () => done();
        speechSynthesis.speak(u);
      }),
    );
  }, [recs, working]);

  return { on: state !== "off", state, heard, toggle };
}

const LABEL: Record<VoiceState, string> = {
  off: "Voice mode",
  wake: "Listening for “Hey PAI”",
  hearing: "Hearing your message",
  sending: "Sending",
  speaking: "Reading the reply",
};

// Hint is what the message box says in voice mode.
export const hint = (s: VoiceState) =>
  s === "wake" ? "Say “Hey PAI”, then your message" : `${LABEL[s]}…`;

// VoiceButtons are the dictate and voice mode buttons of the composer. They
// keep one size in every state, so nothing moves.
export function VoiceButtons({ dictate, voice }: { dictate: ReturnType<typeof useDictation>; voice: ReturnType<typeof useVoiceMode> }) {
  return (
    <>
      <button className={`voice-btn ${dictate.on ? "on" : ""}`} disabled={voice.on} onClick={dictate.toggle} title={dictate.on ? "Stop dictating" : "Dictate"} aria-label="Dictate" aria-pressed={dictate.on}>
        <svg viewBox="0 0 24 24">
          <rect x="9" y="3" width="6" height="11" rx="3" />
          <path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V21" />
        </svg>
      </button>
      <button className={`voice-btn ${voice.state}`} disabled={dictate.on} onClick={voice.toggle} title={voice.on ? `${LABEL[voice.state]}. Tap to end voice mode.` : "Voice mode: say “Hey PAI” and your message. It works while this page is open and the screen is on."} aria-label="Voice mode" aria-pressed={voice.on}>
        <svg viewBox="0 0 24 24">
          <path d="M4 10v4M8 7v10M12 4v16M16 7v10M20 10v4" />
        </svg>
      </button>
    </>
  );
}
