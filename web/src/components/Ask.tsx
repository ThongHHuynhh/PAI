import { useState } from "react";
import { call, type Rec } from "../api";

export type Question = { header: string; question: string; multiSelect?: boolean; options: { label: string; description: string }[] };

// questionsOf is the questions in an ask call record, none for any other call.
export function questionsOf(r: Rec): Question[] {
  try {
    const c = JSON.parse(r.body);
    return c.name === "ask" ? (c.arguments?.questions ?? []) : [];
  } catch {
    return [];
  }
}

// Ask is the popup for an ask call the session waits on: every question with
// its options and a box for another answer. The answers go to the call's
// route; the result record that follows closes it. Dismiss hides it without
// answering, and the call's row in the transcript brings it back.
export function Ask({ session, call: rec, onDismiss }: { session: string; call: Rec; onDismiss: () => void }) {
  const qs = questionsOf(rec);
  const [picked, setPicked] = useState<string[][]>(qs.map(() => []));
  const [other, setOther] = useState<string[]>(qs.map(() => ""));
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const answers = qs.map((q, i) => {
    const text = other[i].trim();
    return [...picked[i], ...(text ? [text] : [])].slice(q.multiSelect ? undefined : -1);
  });
  const ready = answers.every((a) => a.length > 0);

  const pick = (i: number, label: string) =>
    setPicked(picked.map((p, j) => (j !== i ? p : qs[i].multiSelect ? (p.includes(label) ? p.filter((l) => l !== label) : [...p, label]) : [label])));
  // a single choice has one answer: typing replaces the option, picking one clears the text
  const type = (i: number, text: string) => {
    setOther(other.map((o, j) => (j === i ? text : o)));
    if (text.trim() && !qs[i].multiSelect) setPicked(picked.map((p, j) => (j === i ? [] : p)));
  };

  const send = async (declined: boolean) => {
    setBusy(true);
    setErr("");
    try {
      await call("POST", `/sessions/${session}/answer?via=web`, { call: rec.id, ...(declined ? { declined } : { answers }) });
    } catch (e: any) {
      setErr(String(e.message ?? e));
      setBusy(false);
    }
  };

  return (
    <div className="modal-back" onClick={onDismiss}>
      <div className="modal ask" role="dialog" aria-label="PAI asks" onClick={(e) => e.stopPropagation()}>
        <h2>PAI asks</h2>
        {qs.map((q, i) => (
          <fieldset key={i} className="ask-q">
            <legend>
              <span className="ask-head">{q.header}</span>
              {q.multiSelect && <span className="muted small"> pick any number</span>}
            </legend>
            <p>{q.question}</p>
            {q.options.map((o) => (
              <label key={o.label} className={`ask-opt ${picked[i].includes(o.label) ? "on" : ""}`}>
                <input type={q.multiSelect ? "checkbox" : "radio"} name={`q${i}`} checked={picked[i].includes(o.label)} onChange={() => pick(i, o.label)} />
                <span>
                  <b>{o.label}</b>
                  <span className="muted small">{o.description}</span>
                </span>
              </label>
            ))}
            <input className="ask-other" value={other[i]} placeholder="Other: type your own answer" onChange={(e) => type(i, e.target.value)} />
          </fieldset>
        ))}
        {err && <p className="error">{err}</p>}
        <div className="row end">
          <button className="ghost" disabled={busy} onClick={() => send(true)}>
            Decline
          </button>
          <button className="primary" disabled={busy || !ready} onClick={() => send(false)}>
            Send answers
          </button>
        </div>
      </div>
    </div>
  );
}
