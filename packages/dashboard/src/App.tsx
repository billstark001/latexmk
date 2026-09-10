import type { ComponentChildren } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import {
  Activity,
  CheckCircle2,
  Clock3,
  Copy,
  Database,
  FileArchive,
  KeyRound,
  LoaderCircle,
  LogIn,
  Plus,
  RefreshCw,
  Server,
  Settings2,
  ShieldCheck,
  Users,
  XCircle,
} from 'lucide-preact';
import { ApiClient } from './api';
import type { Job, Metadata, User } from './types';

type View = 'jobs' | 'members' | 'service';

const SAVED_ENDPOINT = 'latexmk.console.endpoint';
const SAVED_TOKEN = 'latexmk.console.token';

function initial(name: string, fallback = ''): string {
  return localStorage.getItem(name) ?? fallback;
}

export function App() {
  const [endpoint, setEndpoint] = useState(() => initial(SAVED_ENDPOINT, import.meta.env.VITE_API_BASE_URL || ''));
  const [token, setToken] = useState(() => initial(SAVED_TOKEN));
  const [endpointInput, setEndpointInput] = useState(endpoint);
  const [tokenInput, setTokenInput] = useState(token);
  const [view, setView] = useState<View>('jobs');
  const [meta, setMeta] = useState<Metadata>();
  const [jobs, setJobs] = useState<Job[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const [issuedToken, setIssuedToken] = useState('');
  const [newUser, setNewUser] = useState({ name: '', email: '', role: 'member' });

  const client = useMemo(() => new ApiClient(endpoint.replace(/\/$/, ''), token), [endpoint, token]);

  const refresh = async (includeUsers = view === 'members') => {
    setLoading(true);
    setError('');
    try {
      const [nextMeta, nextJobs] = await Promise.all([client.meta(), client.jobs()]);
      setMeta(nextMeta);
      setJobs(nextJobs.jobs);
      if (includeUsers) {
        const response = await client.users();
        setUsers(response.users);
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not connect to service');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(false), 4_000);
    return () => window.clearInterval(timer);
    // Refreshing when credentials change is intentional; client is memoized.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client]);

  const connect = () => {
    localStorage.setItem(SAVED_ENDPOINT, endpointInput);
    localStorage.setItem(SAVED_TOKEN, tokenInput);
    setEndpoint(endpointInput);
    setToken(tokenInput);
    setMessage('Connection settings saved.');
    if (endpointInput === endpoint && tokenInput === token) void refresh();
  };

  const changeView = (next: View) => {
    setView(next);
    if (next === 'members') void refresh(true);
  };

  const cancel = async (job: Job) => {
    try {
      await client.cancelJob(job.id);
      setMessage(`Cancelled ${job.id}.`);
      await refresh(false);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not cancel job');
    }
  };

  const addUser = async (event: Event) => {
    event.preventDefault();
    try {
      await client.createUser(newUser);
      setNewUser({ name: '', email: '', role: 'member' });
      setMessage('Member created.');
      await refresh(true);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not create member');
    }
  };

  const toggleUser = async (user: User) => {
    try {
      await client.setUserEnabled(user.id, !user.enabled);
      await refresh(true);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not update member');
    }
  };

  const issueToken = async (user: User) => {
    const name = window.prompt(`Create a token for ${user.name}. Token name:`, 'laptop');
    if (name === null) return;
    try {
      const response = await client.createToken(user.id, name);
      setIssuedToken(response.token);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not create token');
    }
  };

  const running = jobs.filter((job) => job.status === 'running').length;
  const queued = jobs.filter((job) => job.status === 'queued').length;
  const completed = jobs.filter((job) => job.status === 'succeeded').length;

  return (
    <div class="app-shell">
      <aside class="sidebar">
        <div class="brand">
          <span class="brand-mark">λ</span>
          <span>latexmk</span>
        </div>
        <p class="eyebrow">REMOTE COMPILATION</p>
        <nav aria-label="Console navigation">
          <NavButton
            active={view === 'jobs'}
            onClick={() => changeView('jobs')}
            icon={<Activity size={17} />}
            label="Job queue"
          />
          <NavButton
            active={view === 'members'}
            onClick={() => changeView('members')}
            icon={<Users size={17} />}
            label="Members and tokens"
          />
          <NavButton
            active={view === 'service'}
            onClick={() => changeView('service')}
            icon={<Settings2 size={17} />}
            label="Service capabilities"
          />
        </nav>
        <div class="sidebar-note">
          <ShieldCheck size={18} />
          <span>Source files sync incrementally by content hash; jobs run in isolated workspaces.</span>
        </div>
      </aside>

      <main class="main-content">
        <header class="topbar">
          <div>
            <p class="eyebrow">CONTROL ROOM</p>
            <h1>
              {view === 'jobs'
                ? 'Compilation jobs'
                : view === 'members'
                  ? 'Members and access tokens'
                  : 'Service capabilities'}
            </h1>
          </div>
          <button
            class="icon-button"
            onClick={() => void refresh(view === 'members')}
            aria-label="Refresh data"
            title="Refresh data"
          >
            <RefreshCw size={18} class={loading ? 'spin' : ''} />
          </button>
        </header>

        <section class="connection-card" aria-label="Connection settings">
          <div class="connection-title">
            <Server size={17} />
            <strong>Server connection</strong>
            <span class={meta ? 'status-dot online' : 'status-dot'} /> {meta ? 'Connected' : 'Disconnected'}
          </div>
          <label>
            Server URL
            <input
              value={endpointInput}
              onInput={(event) => setEndpointInput(event.currentTarget.value)}
              placeholder="Leave empty to use same-origin /v1"
            />
          </label>
          <label>
            Access token
            <input
              value={tokenInput}
              onInput={(event) => setTokenInput(event.currentTarget.value)}
              type="password"
              placeholder="Bearer token (empty only in none mode)"
            />
          </label>
          <button class="button primary" onClick={connect}>
            <LogIn size={16} />
            Connect
          </button>
        </section>

        {error && (
          <div class="notice error">
            <XCircle size={18} />
            <span>{error}</span>
            <button onClick={() => setError('')}>Dismiss</button>
          </div>
        )}
        {message && (
          <div class="notice success">
            <CheckCircle2 size={18} />
            <span>{message}</span>
            <button onClick={() => setMessage('')}>Dismiss</button>
          </div>
        )}

        {view === 'jobs' && (
          <JobsView
            jobs={jobs}
            running={running}
            queued={queued}
            completed={completed}
            onCancel={(job) => void cancel(job)}
            onDownload={(job) =>
              void client
                .downloadResult(job)
                .catch((cause) => setError(cause instanceof Error ? cause.message : 'Could not download result'))
            }
          />
        )}
        {view === 'members' && (
          <MembersView
            users={users}
            newUser={newUser}
            setNewUser={setNewUser}
            onSubmit={(event) => void addUser(event)}
            onToggle={(user) => void toggleUser(user)}
            onToken={(user) => void issueToken(user)}
          />
        )}
        {view === 'service' && <ServiceView meta={meta} />}
      </main>

      {issuedToken && <TokenDialog token={issuedToken} onClose={() => setIssuedToken('')} />}
    </div>
  );
}

function NavButton(props: { active: boolean; icon: ComponentChildren; label: string; onClick: () => void }) {
  return (
    <button class={props.active ? 'nav-button active' : 'nav-button'} onClick={props.onClick}>
      {props.icon}
      <span>{props.label}</span>
    </button>
  );
}

function JobsView(props: {
  jobs: Job[];
  running: number;
  queued: number;
  completed: number;
  onCancel: (job: Job) => void;
  onDownload: (job: Job) => void;
}) {
  return (
    <>
      <section class="metrics">
        <Metric label="Running" value={props.running} icon={<LoaderCircle size={18} />} tone="blue" />
        <Metric label="Queued" value={props.queued} icon={<Clock3 size={18} />} tone="amber" />
        <Metric label="Completed" value={props.completed} icon={<CheckCircle2 size={18} />} tone="green" />
      </section>
      <section class="panel">
        <div class="panel-heading">
          <div>
            <p class="eyebrow">LIVE QUEUE</p>
            <h2>Job history</h2>
          </div>
          <span class="count-pill">{props.jobs.length} items</span>
        </div>
        {props.jobs.length === 0 ? (
          <EmptyState
            icon={<FileArchive size={25} />}
            title="No compilation jobs yet"
            description="Submit a project with the local latexmk CLI; queue, runtime, and result status will appear here."
          />
        ) : (
          <div class="job-list">
            {props.jobs.map((job) => (
              <JobRow
                key={job.id}
                job={job}
                onCancel={() => props.onCancel(job)}
                onDownload={() => props.onDownload(job)}
              />
            ))}
          </div>
        )}
      </section>
    </>
  );
}

function JobRow({ job, onCancel, onDownload }: { job: Job; onCancel: () => void; onDownload: () => void }) {
  const ready = job.status === 'succeeded' || job.status === 'failed';
  return (
    <article class="job-row">
      <div class="job-main">
        <span class={`job-state ${job.status}`}>{labelStatus(job.status)}</span>
        <div>
          <strong>{job.projectId}</strong>
          <code>{job.id}</code>
        </div>
      </div>
      <div class="job-detail">
        <span>{formatTime(job.createdAt)}</span>
        {job.result && (
          <span>
            {job.result.engine} · {formatDuration(job.result.durationMs)}
          </span>
        )}
      </div>
      <div class="job-actions">
        {job.status === 'queued' && (
          <button class="button subtle" onClick={onCancel}>
            Cancel
          </button>
        )}
        {ready && (
          <button class="button subtle" onClick={onDownload}>
            Download result
          </button>
        )}
      </div>
      {job.error && <p class="job-error">{job.error}</p>}
    </article>
  );
}

function MembersView(props: {
  users: User[];
  newUser: { name: string; email: string; role: string };
  setNewUser: (value: { name: string; email: string; role: string }) => void;
  onSubmit: (event: Event) => void;
  onToggle: (user: User) => void;
  onToken: (user: User) => void;
}) {
  return (
    <div class="two-column">
      <section class="panel">
        <div class="panel-heading">
          <div>
            <p class="eyebrow">ACCESS CONTROL</p>
            <h2>Research group members</h2>
          </div>
          <span class="count-pill">{props.users.length} members</span>
        </div>
        {props.users.length === 0 ? (
          <EmptyState
            icon={<Users size={25} />}
            title="No visible members"
            description="This page requires an administrator token and database mode."
          />
        ) : (
          <div class="member-list">
            {props.users.map((user) => (
              <article class="member-row" key={user.id}>
                <div class="avatar">{user.name.slice(0, 1).toUpperCase()}</div>
                <div class="member-name">
                  <strong>{user.name}</strong>
                  <span>{user.email || 'No email set'}</span>
                </div>
                <span class={`role ${user.role}`}>{user.role}</span>
                <button class="text-button" onClick={() => props.onToken(user)}>
                  <KeyRound size={15} />
                  Token
                </button>
                <button class="text-button" onClick={() => props.onToggle(user)}>
                  {user.enabled ? 'Disable' : 'Enable'}
                </button>
              </article>
            ))}
          </div>
        )}
      </section>
      <section class="panel form-panel">
        <p class="eyebrow">NEW MEMBER</p>
        <h2>Add member</h2>
        <form onSubmit={props.onSubmit}>
          <label>
            Name
            <input
              required
              value={props.newUser.name}
              onInput={(event) => props.setNewUser({ ...props.newUser, name: event.currentTarget.value })}
            />
          </label>
          <label>
            Email
            <input
              type="email"
              value={props.newUser.email}
              onInput={(event) => props.setNewUser({ ...props.newUser, email: event.currentTarget.value })}
            />
          </label>
          <label>
            Role
            <select
              value={props.newUser.role}
              onChange={(event) => props.setNewUser({ ...props.newUser, role: event.currentTarget.value })}
            >
              <option value="member">Member</option>
              <option value="admin">Administrator</option>
            </select>
          </label>
          <button class="button primary" type="submit">
            <Plus size={16} />
            Create member
          </button>
        </form>
      </section>
    </div>
  );
}

function ServiceView({ meta }: { meta?: Metadata }) {
  if (!meta)
    return (
      <section class="panel">
        <EmptyState
          icon={<Server size={25} />}
          title="Service metadata has not been loaded"
          description="Enter the server URL and token, then connect."
        />
      </section>
    );
  const capability = meta.capabilities;
  return (
    <>
      <section class="metrics service-metrics">
        <Metric label="Image profile" value={meta.imageProfile} icon={<Server size={18} />} tone="blue" />
        <Metric label="Data store" value={meta.database} icon={<Database size={18} />} tone="violet" />
        <Metric
          label="Protocol version"
          value={`v${meta.protocolVersion}`}
          icon={<ShieldCheck size={18} />}
          tone="green"
        />
      </section>
      <section class="two-column">
        <section class="panel">
          <div class="panel-heading">
            <div>
              <p class="eyebrow">SCHEDULER</p>
              <h2>Compilation resources</h2>
            </div>
          </div>
          <dl class="key-values">
            <dt>Available engines</dt>
            <dd>{capability.engines.join(' · ')}</dd>
            <dt>Concurrent compiles</dt>
            <dd>{capability.maxConcurrentCompiles}</dd>
            <dt>Queue limit</dt>
            <dd>{capability.maxQueuedJobs}</dd>
            <dt>Upload sessions</dt>
            <dd>{capability.maxUploadSessions}</dd>
            <dt>Compile timeout</dt>
            <dd>{formatDuration(capability.compileTimeoutMs)}</dd>
            <dt>State volume limit</dt>
            <dd>{formatBytes(capability.maxStateBytes)}</dd>
            <dt>Result retention</dt>
            <dd>{formatDuration(capability.resultRetentionMs)}</dd>
            <dt>Incremental upload</dt>
            <dd>{capability.incrementalUpload ? 'Enabled' : 'Disabled'}</dd>
            <dt>Shell escape</dt>
            <dd>{capability.shellEscapeAllowed ? 'Allowed (use carefully)' : 'Disabled'}</dd>
          </dl>
        </section>
        <section class="panel">
          <div class="panel-heading">
            <div>
              <p class="eyebrow">TOOLCHAIN</p>
              <h2>Typesetting tools</h2>
            </div>
          </div>
          <dl class="tool-list">
            {Object.entries(meta.toolchain).map(([name, version]) => (
              <div key={name}>
                <dt>{name}</dt>
                <dd>{version}</dd>
              </div>
            ))}
          </dl>
        </section>
      </section>
    </>
  );
}

function Metric({
  label,
  value,
  icon,
  tone,
}: {
  label: string;
  value: string | number;
  icon: ComponentChildren;
  tone: string;
}) {
  return (
    <article class={`metric ${tone}`}>
      <span class="metric-icon">{icon}</span>
      <div>
        <span>{label}</span>
        <strong>{value}</strong>
      </div>
    </article>
  );
}
function EmptyState({ icon, title, description }: { icon: ComponentChildren; title: string; description: string }) {
  return (
    <div class="empty-state">
      <span>{icon}</span>
      <h3>{title}</h3>
      <p>{description}</p>
    </div>
  );
}
function TokenDialog({ token, onClose }: { token: string; onClose: () => void }) {
  return (
    <div class="dialog-backdrop" role="presentation">
      <section class="dialog" role="dialog" aria-modal="true" aria-label="New access token">
        <KeyRound size={24} />
        <h2>Save this token now</h2>
        <p>It is shown only once.</p>
        <code>{token}</code>
        <button class="button subtle" onClick={() => void navigator.clipboard.writeText(token)}>
          {' '}
          <Copy size={15} />
          Copy
        </button>
        <button class="button primary" onClick={onClose}>
          I saved it
        </button>
      </section>
    </div>
  );
}
function labelStatus(status: Job['status']) {
  return { queued: 'Queued', running: 'Compiling', succeeded: 'Succeeded', failed: 'Failed', cancelled: 'Cancelled' }[
    status
  ];
}
function formatDuration(milliseconds: number) {
  if (milliseconds >= 86_400_000) return `${(milliseconds / 86_400_000).toFixed(1)} days`;
  if (milliseconds >= 3_600_000) return `${(milliseconds / 3_600_000).toFixed(1)} h`;
  return milliseconds >= 60_000
    ? `${(milliseconds / 60_000).toFixed(1)} min`
    : `${(milliseconds / 1_000).toFixed(1)} s`;
}
function formatBytes(bytes: number) {
  return bytes >= 1 << 30 ? `${(bytes / (1 << 30)).toFixed(1)} GiB` : `${(bytes / (1 << 20)).toFixed(0)} MiB`;
}
function formatTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? value
    : date.toLocaleString('en-US', { hour: '2-digit', minute: '2-digit', month: 'short', day: 'numeric' });
}
