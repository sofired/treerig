import { stat } from 'node:fs/promises';

/** The permission bits of a file or directory, for asserting on modes. */
export const fileMode = async (file: string): Promise<number> => (await stat(file)).mode & 0o777;
