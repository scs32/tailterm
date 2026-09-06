export async function createIPN() {
  throw new Error("Build in static mode to use this runtime.");
}
export const validatePrivateKey = createIPN;
export const generatePrivateKey = createIPN;
