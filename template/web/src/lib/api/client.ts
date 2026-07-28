import createClient from 'openapi-fetch';
import type { paths } from './schema';

// The API and SPA share an origin (the Go binary serves both), so relative
// paths just work. Types come straight from the generated OpenAPI schema.
export const api = createClient<paths>({ baseUrl: '' });
