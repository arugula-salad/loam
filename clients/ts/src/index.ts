// @arugula-salad/loam: a client for loam's public API (docs/API.md), for
// Node 20 and later, without dependencies. pkg/client is the Go one; this
// follows its shape.

export { LoamClient, INSIDE_SOCKET, portURL, portURLs } from "./client.js";
export type { ClientOptions, ListOptions, ForkOptions, EventsOptions } from "./client.js";
export { SandboxApi, InsideSandbox } from "./sandbox.js";
export type { FsOptions, WriteFileOptions, MkdirOptions, RecursiveOptions, RunOptions, RunResult, LogOptions } from "./sandbox.js";
export { ExecStream } from "./exec.js";
export type { ExecOptions, AttachOptions, StreamIO, Sink, StdinSource } from "./exec.js";
export { FileWatch, PortWatch, EventStream } from "./streams.js";
export type { PortEvent } from "./streams.js";
export { LoamError, ExitUnknownError, isCode, isNotFound, errorCode } from "./errors.js";
export type { BodySource, RetryOptions, RequestOptions } from "./http.js";
export * from "./types.js";
