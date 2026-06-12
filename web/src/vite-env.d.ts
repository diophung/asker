/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Keycloak base URL. Default: http://localhost:8081 */
  readonly VITE_KEYCLOAK_URL?: string;
  /** Asker gateway base URL. Default: http://localhost:8080 */
  readonly VITE_API_URL?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
