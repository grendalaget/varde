// Generated from the Rust desktop IPC types by desktop_ipc_bindings.

// prettier-ignore
export type Facet = "setup" | "dashboard";

// prettier-ignore
export type FaceInfo = { facet: Facet, cp_url: string, group_id: string, node_id: string, can_join_cs2: boolean, };

// prettier-ignore
export type JoinCs2ServerRequest = { address: string, };
