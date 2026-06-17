import { useState } from 'react';
import { RFCStatusBadge } from './RFCStatusBadge';

interface Application {
  metadata: { name: string; namespace?: string };
  spec: { destination: { namespace: string }; project: string };
}

interface Resource {
  metadata: {
    name: string;
    namespace?: string;
    annotations?: Record<string, string>;
  };
}

export interface ResourceExtensionProps {
  application: Application;
  resource: Resource;
}

interface ValidationResult {
  approved: boolean;
  rfc_title: string;
  approver: string;
  window_start: string;
  window_end: string;
  reason: string;
}

type FormState = 'idle' | 'checking' | 'approved' | 'rejected' | 'done';


export function RFCValidationTab({ application, resource }: ResourceExtensionProps) {
  const annotations = resource?.metadata?.annotations ?? {};
  const status = annotations['rfc-validation/status'];

  if (status !== 'pending') {
    return <RFCStatusBadge status={status} annotations={annotations} />;
  }

  return <RFCValidationForm application={application} resource={resource} />;
}

function RFCValidationForm({ application, resource }: ResourceExtensionProps) {
  const [rfcId, setRfcId] = useState('');
  const [formState, setFormState] = useState<FormState>('idle');
  const [confirming, setConfirming] = useState(false);
  const [result, setResult] = useState<ValidationResult | null>(null);
  const [error, setError] = useState('');

  const ns = resource.metadata.namespace ?? application.spec.destination.namespace;

  async function handleValidate() {
    if (!rfcId.trim()) return;
    setFormState('checking');
    setError('');

    try {
      const res = await fetch('/extensions/rfc-validator/validate', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Argocd-Application-Name': `${application.metadata.namespace ?? 'argocd'}:${application.metadata.name}`,
          'Argocd-Project-Name': application.spec.project ?? 'default',
        },
        body: JSON.stringify({ rfc_id: rfcId.trim(), namespace: ns }),
      });
      if (!res.ok) {
        const body = await res.text().catch(() => '');
        throw new Error(`Unexpected status ${res.status}${body ? `: ${body}` : ''}`);
      }
      const data: ValidationResult = await res.json();
      setResult(data);
      setFormState(data.approved ? 'approved' : 'rejected');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Validation request failed');
      setFormState('idle');
    }
  }

  async function handleConfirmRestart() {
    if (!result?.approved) return;
    setConfirming(true);

    const appName = application.metadata.name;
    const appNamespace = application.metadata.namespace ?? 'argocd';

    // Trigger the "restart (RFC approved)" Lua action via RunResourceAction.
    // The Lua action sets rfc-validation/status=approved and patches restartedAt
    // on spec.template to trigger the rolling restart — all server-side.
    // This avoids the ArgoCD v3 resource patch API which changed behaviour.
    try {
      const res = await fetch(
        `/api/v1/applications/${encodeURIComponent(appName)}/resource/actions?` +
          `namespace=${encodeURIComponent(ns)}` +
          `&resourceName=${encodeURIComponent(resource.metadata.name)}` +
          `&version=v1&kind=Deployment&group=apps` +
          `&appNamespace=${encodeURIComponent(appNamespace)}`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify('restart (RFC approved)'),
        }
      );
      if (!res.ok) throw new Error(`Action failed: ${res.status} ${await res.text()}`);
      setFormState('done');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Restart patch failed');
      setConfirming(false);
    }
  }

  if (formState === 'done') {
    return (
      <div style={s.container}>
        <p style={{ color: '#1b5e20', marginTop: 0 }}>
          Rolling restart initiated for <strong>{resource.metadata.name}</strong>.
        </p>
      </div>
    );
  }

  return (
    <div style={s.container}>
      <h4 style={s.heading}>RFC Validation Required</h4>
      <p style={s.subtitle}>
        <strong>{resource.metadata.name}</strong> has a pending restart request.
        Enter an approved RFC number to proceed.
      </p>

      <div style={s.row}>
        <input
          type="text"
          placeholder="e.g. CHG0012345"
          value={rfcId}
          onChange={e => setRfcId(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && handleValidate()}
          disabled={formState === 'checking' || confirming}
          style={s.input}
        />
        <button
          onClick={handleValidate}
          disabled={!rfcId.trim() || formState === 'checking' || confirming}
          style={s.validateBtn}
        >
          {formState === 'checking' ? 'Checking…' : 'Validate'}
        </button>
      </div>

      {error && <p style={s.error}>{error}</p>}

      {result && (formState === 'approved' || formState === 'rejected') && (
        <RFCStatusBadge
          status={formState}
          result={result}
        />
      )}

      {formState === 'approved' && (
        <button onClick={handleConfirmRestart} disabled={confirming} style={s.confirmBtn}>
          {confirming ? 'Applying…' : 'Confirm restart'}
        </button>
      )}
    </div>
  );
}

const s: Record<string, React.CSSProperties> = {
  container: { padding: '1.25rem', maxWidth: 520 },
  heading:   { marginTop: 0, marginBottom: '0.5rem', fontSize: '1rem' },
  subtitle:  { color: '#555', fontSize: '0.875rem', marginBottom: '1rem' },
  row:       { display: 'flex', gap: '0.5rem', marginBottom: '0.75rem' },
  input:     { flex: 1, padding: '0.5rem 0.75rem', border: '1px solid #ccc', borderRadius: 4 },
  validateBtn: { padding: '0.5rem 1rem', cursor: 'pointer', borderRadius: 4, border: '1px solid #ccc' },
  error:     { color: '#c00', fontSize: '0.85rem', margin: '0 0 0.5rem' },
  confirmBtn: {
    marginTop: '0.75rem', padding: '0.6rem 1.25rem',
    background: '#2e7d32', color: '#fff', border: 'none',
    borderRadius: 4, cursor: 'pointer', width: '100%', fontSize: '0.9rem',
  },
};
