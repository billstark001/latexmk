import type { Job, Metadata, User } from './types';

export class ApiClient {
  constructor(
    readonly baseUrl: string,
    readonly token: string,
  ) {}

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers);
    headers.set('Accept', 'application/json');
    if (this.token) headers.set('Authorization', `Bearer ${this.token}`);
    if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
    const response = await fetch(`${this.baseUrl}${path}`, { ...init, headers });
    if (!response.ok) {
      const body = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(body.error || `Request failed (${response.status})`);
    }
    if (response.status === 204) return undefined as T;
    return response.json() as Promise<T>;
  }

  meta() {
    return this.request<Metadata>('/v1/meta');
  }
  jobs() {
    return this.request<{ jobs: Job[] }>('/v1/jobs?limit=100');
  }
  job(id: string) {
    return this.request<Job>(`/v1/jobs/${encodeURIComponent(id)}`);
  }
  cancelJob(id: string) {
    return this.request<Job>(`/v1/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' });
  }
  users() {
    return this.request<{ users: User[] }>('/v1/admin/users');
  }
  createUser(body: { name: string; email: string; role: string }) {
    return this.request<User>('/v1/admin/users', { method: 'POST', body: JSON.stringify(body) });
  }
  setUserEnabled(id: string, enabled: boolean) {
    return this.request<void>(`/v1/admin/users/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify({ enabled }),
    });
  }
  createToken(id: string, name: string) {
    return this.request<{ token: string; tokenInfo: { name: string } }>(
      `/v1/admin/users/${encodeURIComponent(id)}/tokens`,
      { method: 'POST', body: JSON.stringify({ name }) },
    );
  }
  async downloadResult(job: Job): Promise<void> {
    const headers = new Headers();
    if (this.token) headers.set('Authorization', `Bearer ${this.token}`);
    const response = await fetch(`${this.baseUrl}/v1/jobs/${encodeURIComponent(job.id)}/result`, { headers });
    if (!response.ok) throw new Error('Could not download job result');
    const blob = await response.blob();
    const link = document.createElement('a');
    link.href = URL.createObjectURL(blob);
    link.download = `${job.projectId}-${job.id}.tar.gz`;
    link.click();
    URL.revokeObjectURL(link.href);
  }
}
