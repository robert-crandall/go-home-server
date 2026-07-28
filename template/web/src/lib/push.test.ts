import { describe, it, expect } from 'vitest';
import { urlBase64ToUint8Array } from './push';

describe('urlBase64ToUint8Array', () => {
  it('decodes a url-safe base64 string to bytes', () => {
    // "SGVsbG8" is base64url for "Hello".
    const bytes = urlBase64ToUint8Array('SGVsbG8');
    expect(Array.from(bytes)).toEqual([72, 101, 108, 108, 111]);
  });

  it('handles url-safe characters', () => {
    // Contains '-' and '_' which are url-safe replacements for '+' and '/'.
    const bytes = urlBase64ToUint8Array('a-_a');
    expect(bytes.length).toBeGreaterThan(0);
  });
});
