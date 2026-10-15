import { Codes, type ErrorBody } from "./types.js";

/**
 * LoamError is an error answer from the API (docs/API.md, "Errors"): a status,
 * the snake_case code clients branch on, and a sentence for people. An answer
 * that isn't the documented shape (a proxy's error page, say) still becomes
 * one, with the code `http_<status>` and the body as the message, so callers
 * have a single type to look for.
 */
export class LoamError extends Error {
  readonly status: number;
  readonly code: string;
  /** The server's Retry-After in seconds, set on `capacity` errors. */
  readonly retryAfter?: number;

  constructor(status: number, code: string, message: string, retryAfter?: number) {
    super(message);
    this.name = "LoamError";
    this.status = status;
    this.code = code;
    if (retryAfter !== undefined) this.retryAfter = retryAfter;
  }

  /** isCode reports whether the error's code is one of codes. */
  isCode(...codes: string[]): boolean {
    return codes.includes(this.code);
  }

  override toString(): string {
    return `${this.name}: ${this.message} (${this.code}, ${this.status})`;
  }
}

/** isCode reports whether err is a LoamError with one of codes. */
export function isCode(err: unknown, ...codes: string[]): err is LoamError {
  return err instanceof LoamError && err.isCode(...codes);
}

/** errorCode is err's API error code, or "" when it has none. */
export function errorCode(err: unknown): string {
  return err instanceof LoamError ? err.code : "";
}

/**
 * isNotFound reports whether err is the API's 404. Another project's sandbox
 * answers this too: it is how isolation looks from outside.
 */
export function isNotFound(err: unknown): err is LoamError {
  return err instanceof LoamError && (err.code === Codes.NotFound || err.status === 404);
}

/**
 * ExitUnknownError is what a stream's wait() rejects with when it ended
 * without an exit frame: the sandbox went away under the session (deleted,
 * restored, its node lost; close status 1011), or the connection broke. It is
 * never success.
 */
export class ExitUnknownError extends Error {
  readonly code = "exit_unknown";
  /** The WebSocket close status, when the server sent one. */
  readonly closeCode?: number;

  constructor(detail: string, closeCode?: number) {
    super(`the session ended without an exit status${detail ? ` (${detail})` : ""}`);
    this.name = "ExitUnknownError";
    if (closeCode !== undefined) this.closeCode = closeCode;
  }
}

/**
 * errorFromBody turns a non-2xx answer's status, headers and body into a
 * LoamError, as readError does in the Go client.
 */
export function errorFromBody(status: number, statusText: string, retryAfter: string | null, raw: string): LoamError {
  let code = "";
  let message = "";
  try {
    const body = JSON.parse(raw) as Partial<ErrorBody>;
    if (body && typeof body.error?.code === "string" && body.error.code !== "") {
      code = body.error.code;
      message = typeof body.error.message === "string" ? body.error.message : "";
    }
  } catch {
    // Not JSON: the body is the message.
  }
  if (code === "") {
    code = `http_${status}`;
    message = raw.trim() || `${status} ${statusText}`.trim();
  }
  let after: number | undefined;
  if (retryAfter !== null && /^\d+$/.test(retryAfter.trim())) {
    after = Number(retryAfter.trim());
  }
  return new LoamError(status, code, message, after);
}
