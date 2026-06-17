interface ValidationResult {
  rfc_title?: string;
  approver?: string;
  window_end?: string;
  reason?: string;
}

interface Props {
  status?: string;
  result?: ValidationResult;
  annotations?: Record<string, string>;
}

const STATUS_STYLE: Record<string, { bg: string; border: string; color: string; label: string }> = {
  pending:  { bg: '#fff8e1', border: '#ffe082', color: '#f57f17', label: 'Pending — awaiting RFC input' },
  approved: { bg: '#e8f5e9', border: '#a5d6a7', color: '#1b5e20', label: 'Approved' },
  rejected: { bg: '#ffebee', border: '#ef9a9a', color: '#b71c1c', label: 'Rejected' },
};

export function RFCStatusBadge({ status, result, annotations }: Props) {
  if (!status) {
    return (
      <div style={{ padding: '1rem', color: '#666', fontSize: '0.875rem' }}>
        No RFC validation in progress. Right-click this Deployment and select
        {' '}<em>restart (RFC required)</em> to begin.
      </div>
    );
  }

  const cfg = STATUS_STYLE[status] ?? {
    bg: '#f5f5f5', border: '#e0e0e0', color: '#333', label: status,
  };

  const rfcId   = annotations?.['rfc-validation/rfc-id'];
  const approver = result?.approver ?? annotations?.['rfc-validation/approver'];
  const title    = result?.rfc_title;
  const reason   = result?.reason;
  const windowEnd = result?.window_end;

  function fmt(iso?: string) {
    if (!iso) return '';
    return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
  }

  return (
    <div style={{
      padding: '0.75rem 1rem', background: cfg.bg,
      border: `1px solid ${cfg.border}`, borderRadius: 4, margin: '0.5rem 0',
    }}>
      <strong style={{ color: cfg.color }}>{cfg.label}</strong>
      {rfcId    && <p style={row}><strong>RFC:</strong> {rfcId}{title ? ` — ${title}` : ''}</p>}
      {approver && <p style={row}><strong>Approver:</strong> {approver}</p>}
      {windowEnd && <p style={row}><strong>Window closes:</strong> {fmt(windowEnd)}</p>}
      {reason   && <p style={{ ...row, color: cfg.color }}>{reason}</p>}
    </div>
  );
}

const row: React.CSSProperties = { margin: '0.25rem 0 0', fontSize: '0.85rem', color: '#333' };
