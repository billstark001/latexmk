package api

import "time"

// ProtocolVersion 2 adds content-addressed project upload and queued jobs.
// The synchronous /v1/compile endpoint remains available for v1 clients.
const ProtocolVersion = 2

type CompileRequest struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Entry           string `json:"entry"`
	Engine          string `json:"engine"`
	Interaction     string `json:"interaction"`
	Synctex         bool   `json:"synctex"`
	HaltOnError     bool   `json:"haltOnError"`
	FileLineError   bool   `json:"fileLineError"`
	ShellEscape     bool   `json:"shellEscape"`
	JobName         string `json:"jobName,omitempty"`
	Force           bool   `json:"force,omitempty"`
	Quiet           bool   `json:"quiet,omitempty"`
	RecordInputs    bool   `json:"recordInputs,omitempty"`
}

type Artifact struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type CompileResult struct {
	ProtocolVersion int        `json:"protocolVersion"`
	RequestID       string     `json:"requestId"`
	Success         bool       `json:"success"`
	ExitCode        int        `json:"exitCode"`
	TimedOut        bool       `json:"timedOut"`
	DurationMS      int64      `json:"durationMs"`
	Entry           string     `json:"entry"`
	Engine          string     `json:"engine"`
	ServerVersion   string     `json:"serverVersion"`
	ImageProfile    string     `json:"imageProfile"`
	Artifacts       []Artifact `json:"artifacts"`
	InputFiles      []string   `json:"inputFiles,omitempty"`
	StdoutTruncated bool       `json:"stdoutTruncated"`
	StderrTruncated bool       `json:"stderrTruncated"`
	Error           string     `json:"error,omitempty"`
}

type ProjectFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type UploadPlanRequest struct {
	ProjectID string         `json:"projectId"`
	Request   CompileRequest `json:"request"`
	Files     []ProjectFile  `json:"files"`
}

type UploadPlan struct {
	UploadID  string    `json:"uploadId"`
	Missing   []string  `json:"missing"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Job struct {
	ID         string         `json:"id"`
	ProjectID  string         `json:"projectId"`
	SnapshotID string         `json:"snapshotId,omitempty"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"createdAt"`
	StartedAt  *time.Time     `json:"startedAt,omitempty"`
	FinishedAt *time.Time     `json:"finishedAt,omitempty"`
	Result     *CompileResult `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type Metadata struct {
	ProtocolVersion int               `json:"protocolVersion"`
	Service         string            `json:"service"`
	Version         string            `json:"version"`
	Commit          string            `json:"commit"`
	BuildDate       string            `json:"buildDate"`
	ImageProfile    string            `json:"imageProfile"`
	AuthMode        string            `json:"authMode"`
	Database        string            `json:"database"`
	Capabilities    Capabilities      `json:"capabilities"`
	Toolchain       map[string]string `json:"toolchain"`
	Runtime         map[string]string `json:"runtime"`
	Timestamp       time.Time         `json:"timestamp"`
}

type Capabilities struct {
	Engines             []string `json:"engines"`
	MaxUploadBytes      int64    `json:"maxUploadBytes"`
	MaxExpandedBytes    int64    `json:"maxExpandedBytes"`
	MaxFiles            int      `json:"maxFiles"`
	MaxArtifactBytes    int64    `json:"maxArtifactBytes"`
	CompileTimeoutMS    int64    `json:"compileTimeoutMs"`
	MaxConcurrent       int      `json:"maxConcurrentCompiles"`
	ShellEscapeAllowed  bool     `json:"shellEscapeAllowed"`
	ProjectRCFilesRead  bool     `json:"projectRcFilesRead"`
	PersistentWorkspace bool     `json:"persistentWorkspace"`
	IncrementalUpload   bool     `json:"incrementalUpload"`
	QueuedJobs          bool     `json:"queuedJobs"`
	DependencyInputs    bool     `json:"dependencyInputs"`
	MaxQueuedJobs       int      `json:"maxQueuedJobs"`
	MaxStateBytes       int64    `json:"maxStateBytes"`
	MaxUploadSessions   int      `json:"maxUploadSessions"`
	ResultRetentionMS   int64    `json:"resultRetentionMs"`
	SnapshotRetentionMS int64    `json:"snapshotRetentionMs"`
	BlobRetentionMS     int64    `json:"blobRetentionMs"`
}
