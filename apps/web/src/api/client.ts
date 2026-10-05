import createClient from "openapi-fetch";
import type { components, paths } from "./schema";

export const api = createClient<paths>({
  baseUrl: "/",
  credentials: "same-origin",
});

export type Schemas = components["schemas"];
export type Server = Schemas["Server"];
export type Node = Schemas["Node"];
export type Snapshot = Schemas["Snapshot"];
export type Execution = Schemas["Execution"];
export type Game = Schemas["Game"];
export type GameConfigField = Schemas["GameConfigField"];
export type Group = Schemas["Group"];
export type Member = Schemas["Member"];
export type Me = Schemas["Me"];
export type VardeEvent = Schemas["Event"];
export type LogLine = Schemas["LogLine"];
export type EnrollmentToken = Schemas["EnrollmentToken"];
export type ApiError = Schemas["Error"];

/** Extracts a human-readable message from an openapi-fetch error payload. */
export function errorMessage(err: unknown): string {
  if (err && typeof err === "object" && "message" in err) {
    return String((err as { message: unknown }).message);
  }
  return "Something went wrong";
}
