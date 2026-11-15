# Unified Container Desktop frontend

Read README.md and package.json before editing. Preserve the shared production
desktop/browser interface and its real controller operations.
Keep app composition above feature modules and generic components. Add libraries
for an actual need and preserve the framework and design conventions.

Use npm run check for strict types, lint, and component tests; npm run build for
production assets; npm run test:e2e for navigation, keyboard, medium-dark appearance, and browser checks.
Keep one medium-dark palette in the initial HTML/CSS; do not add a light/system
mode switch unless the user requests it. Preserve forced-colors accessibility.
Keep user permission and agent policies. Do not introduce fake actions or claim a
backend, authentication service, or deployed environment exists without integration.
