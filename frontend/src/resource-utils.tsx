import type { Engine } from './api';

export function InventoryErrors({
  errors,
}: {
  errors?: { engine: string; message: string }[];
}) {
  return (
    <>
      {errors?.map((error) => (
        <div className="inline-error" key={error.engine}>
          {error.engine}: {error.message}
        </div>
      ))}
    </>
  );
}

export function EngineSelect({
  value,
  onChange,
  disabled = false,
}: {
  value: Engine;
  onChange: (engine: Engine) => void;
  disabled?: boolean;
}) {
  return (
    <label>
      Engine
      <select
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value as Engine)}
      >
        <option value="linux">Linux</option>
        <option value="windows">Windows</option>
      </select>
    </label>
  );
}

export function lines(value: string): string[] {
  return value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean);
}

export function pairs(value: string): Record<string, string> {
  const result: Record<string, string> = {};
  for (const line of value.split(/\r?\n/).filter((entry) => entry.trim())) {
    const separator = line.indexOf('=');
    if (separator < 1 || !line.slice(0, separator).trim())
      throw new Error('Expected one KEY=VALUE entry per line.');
    result[line.slice(0, separator).trim()] = line.slice(separator + 1);
  }
  return result;
}

export function decodeText(data: string): string {
  return new TextDecoder('utf-8', { fatal: true }).decode(
    Uint8Array.from(atob(data), (character) => character.charCodeAt(0)),
  );
}

export function encodeText(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = '';
  for (let i = 0; i < bytes.length; i += 0x8000)
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(binary);
}
