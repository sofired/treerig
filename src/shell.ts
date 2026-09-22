/** Single-quoted for a POSIX shell, so a pasted command survives any character in the path. */
export function shellQuote(value: string): string {
  return `'${value.replaceAll("'", String.raw`'\''`)}'`;
}
