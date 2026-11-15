// mockup: none — 턴 중 질문 카드(AC-E2)는 SPA에만 있고 대응 목업이 아직 없다
// AC-E2 — a claude-code turn's AskUserQuestion, answered while the turn waits.
// One card answers one call: every question at once, one answer each, keyed by
// the question's text. A multi-select answer is the chosen labels joined by
// ", "; text typed in "Other" is the person's own answer.
import { useState } from "react";
import { api, ApiError } from "../api/client";
import type { PendingQuestion, Question } from "../api/types";

interface Choice {
  labels: string[];
  other: string;
}

function answerFor(question: Question, choice: Choice): string {
  const other = choice.other.trim();
  if (!question.multiSelect) return other !== "" ? other : choice.labels[0] ?? "";
  return [...choice.labels, ...(other !== "" ? [other] : [])].join(", ");
}

export function QuestionCard({
  sessionId,
  pending,
  onAnswered,
  onGone,
}: {
  sessionId: string;
  pending: PendingQuestion;
  onAnswered: () => void;
  onGone: (message: string) => void;
}) {
  const [choices, setChoices] = useState<Choice[]>(() =>
    pending.questions.map(() => ({ labels: [], other: "" })),
  );
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const answers = pending.questions.map((q, i) => answerFor(q, choices[i]));
  const complete = answers.every((a) => a.trim() !== "");

  function update(index: number, next: (c: Choice) => Choice) {
    setChoices((current) => current.map((c, i) => (i === index ? next(c) : c)));
  }

  function pick(index: number, question: Question, label: string) {
    update(index, (c) => {
      if (!question.multiSelect) return { labels: [label], other: "" };
      const labels = c.labels.includes(label)
        ? c.labels.filter((l) => l !== label)
        : [...c.labels, label];
      return { ...c, labels };
    });
  }

  async function submit() {
    if (!complete || sending) return;
    setSending(true);
    setError(null);
    const body: Record<string, string> = {};
    pending.questions.forEach((q, i) => {
      body[q.question] = answers[i];
    });
    try {
      await api.answerQuestion(sessionId, pending.id, body);
      onAnswered();
    } catch (answerError) {
      if (answerError instanceof ApiError && answerError.status === 404) {
        onGone("The turn stopped waiting for this answer");
        return;
      }
      setError(String(answerError));
      setSending(false);
    }
  }

  return (
    <form
      className="question-card"
      data-testid="ws-question"
      data-question-id={pending.id}
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <div className="question-head">
        <span className="eyebrow">The agent is waiting for your answer</span>
      </div>
      {pending.questions.map((question, index) => (
        <fieldset className="question" key={question.question}>
          <legend>
            {question.header ? <span className="question-tag">{question.header}</span> : null}
            {question.question}
          </legend>
          <div className="question-options">
            {question.options.map((option) => {
              const chosen =
                choices[index].labels.includes(option.label) &&
                (question.multiSelect || choices[index].other.trim() === "");
              return (
                <label
                  key={option.label}
                  className={"question-option" + (chosen ? " chosen" : "")}
                >
                  <input
                    type={question.multiSelect ? "checkbox" : "radio"}
                    name={`${pending.id}-${index}`}
                    checked={chosen}
                    onChange={() => pick(index, question, option.label)}
                    disabled={sending}
                  />
                  <span className="question-label">{option.label}</span>
                  {option.description ? (
                    <span className="question-desc">{option.description}</span>
                  ) : null}
                </label>
              );
            })}
          </div>
          <input
            className="question-other"
            data-testid="ws-question-other"
            value={choices[index].other}
            onChange={(event) => {
              const other = event.target.value;
              update(index, (c) => ({ ...c, other }));
            }}
            placeholder={
              question.multiSelect
                ? "Other — added to the chosen options"
                : "Other — your own answer instead"
            }
            aria-label={`Other answer to: ${question.question}`}
            disabled={sending}
            autoComplete="off"
          />
        </fieldset>
      ))}
      <div className="question-foot">
        {error ? (
          <span className="question-error" role="alert">
            {error}
          </span>
        ) : null}
        <button
          type="submit"
          className="agent-send"
          data-testid="ws-question-submit"
          disabled={!complete || sending}
        >
          {sending ? "Sending…" : "Answer"}
        </button>
      </div>
    </form>
  );
}
