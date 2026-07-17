export interface CapabilitySet {
  engines: string[];
  maxUploadBytes: number;
  maxExpandedBytes: number;
  maxFiles: number;
  maxArtifactBytes: number;
  compileTimeoutMs: number;
  maxConcurrentCompiles: number;
  shellEscapeAllowed: boolean;
  incrementalUpload: boolean;
  queuedJobs: boolean;
  dependencyInputs: boolean;
  maxQueuedJobs: number;
  maxStateBytes: number;
  maxUploadSessions: number;
  resultRetentionMs: number;
  snapshotRetentionMs: number;
  blobRetentionMs: number;
}

export interface Metadata {
  protocolVersion: number;
  service: string;
  version: string;
  commit: string;
  buildDate: string;
  imageProfile: string;
  authMode: string;
  database: string;
  capabilities: CapabilitySet;
  toolchain: Record<string, string>;
}

export interface CompileResult {
  success: boolean;
  exitCode: number;
  durationMs: number;
  entry: string;
  engine: string;
  error?: string;
  artifacts: { path: string; size: number; sha256: string }[];
  inputFiles?: string[];
}

export interface Job {
  id: string;
  projectId: string;
  snapshotId?: string;
  status: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled';
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  result?: CompileResult;
  error?: string;
}

export interface User {
  id: string;
  name: string;
  email?: string;
  role: 'admin' | 'member';
  enabled: boolean;
  createdAt: string;
}
