import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { errorText } from './api';

export function useOperation() {
  const [pending, setPending] = useState('');
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');
  async function run(
    label: string,
    action: () => Promise<void>,
    done?: string,
  ): Promise<boolean> {
    setPending(label);
    setError('');
    setSuccess('');
    try {
      await action();
      if (done) setSuccess(done);
      return true;
    } catch (reason) {
      setError(errorText(reason));
      return false;
    } finally {
      setPending('');
    }
  }
  return {
    pending,
    error,
    success,
    run,
    clear: () => {
      setError('');
      setSuccess('');
    },
  };
}

export function Feedback({
  error,
  success,
  pending,
}: {
  error?: string;
  success?: string;
  pending?: string;
}) {
  return (
    <>
      {error && (
        <div className="notice error" role="alert">
          <strong>Unable to complete request</strong>
          <span>{error}</span>
        </div>
      )}
      {success && (
        <div className="notice success" role="status">
          {success}
        </div>
      )}
      {pending && (
        <div className="notice progress" role="status">
          {pending}…
        </div>
      )}
    </>
  );
}

export function LoadState({
  loading,
  error,
  retry,
}: {
  loading: boolean;
  error: string;
  retry: () => void;
}) {
  return (
    <>
      {error && (
        <div className="notice error" role="alert">
          <span>{error}</span>
          <button className="text-button" onClick={retry}>
            Retry
          </button>
        </div>
      )}
      {loading && (
        <p className="loading" role="status">
          Loading…
        </p>
      )}
    </>
  );
}

export function Modal({
  title,
  children,
  close,
}: {
  title: string;
  children: ReactNode;
  close: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const dialog = ref.current;
    const opener =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    dialog?.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      if (opener?.isConnected) opener.focus();
      else document.querySelector<HTMLElement>('main')?.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      aria-label={title}
      onCancel={(event) => {
        event.preventDefault();
        close();
      }}
    >
      <div className="modal-head">
        <h2>{title}</h2>
        <button className="icon-button" aria-label="Close" onClick={close}>
          ×
        </button>
      </div>
      {children}
    </dialog>
  );
}

export function Confirm({
  title,
  detail,
  action,
  pending,
  error,
  close,
  confirm,
}: {
  title: string;
  detail: ReactNode;
  action: string;
  pending: boolean;
  error?: string;
  close: () => void;
  confirm: () => void;
}) {
  return (
    <Modal title={title} close={close}>
      <Feedback error={error} />
      <div className="muted">{detail}</div>
      <div className="modal-actions">
        <button className="button secondary" disabled={pending} onClick={close}>
          Cancel
        </button>
        <button className="button danger" disabled={pending} onClick={confirm}>
          {pending ? 'Working…' : action}
        </button>
      </div>
    </Modal>
  );
}

export function Badge({
  children,
  tone = 'neutral',
}: {
  children: ReactNode;
  tone?: 'good' | 'warn' | 'bad' | 'neutral';
}) {
  return (
    <span className={`badge ${tone}`}>
      <span className="badge-dot" />
      {children}
    </span>
  );
}
export function Empty({
  title,
  children,
}: {
  title: string;
  children?: ReactNode;
}) {
  return (
    <div className="empty">
      <strong>{title}</strong>
      {children && <p>{children}</p>}
    </div>
  );
}
export function SectionTitle({
  eyebrow,
  title,
  description,
  children,
}: {
  eyebrow: string;
  title: string;
  description: string;
  children?: ReactNode;
}) {
  return (
    <header className="page-heading">
      <div>
        <div className="eyebrow">{eyebrow}</div>
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      <div className="heading-actions">{children}</div>
    </header>
  );
}
export function formatResult(value: unknown): string {
  if (typeof value === 'string') return value;
  if (
    value &&
    typeof value === 'object' &&
    'text' in value &&
    typeof value.text === 'string'
  )
    return value.text;
  return JSON.stringify(value, null, 2);
}
export function OutputModal({
  title,
  value,
  close,
}: {
  title: string;
  value: unknown;
  close: () => void;
}) {
  return (
    <Modal title={title} close={close}>
      <pre className="output">{formatResult(value)}</pre>
      <div className="modal-actions">
        <button className="button secondary" onClick={close}>
          Close
        </button>
      </div>
    </Modal>
  );
}
