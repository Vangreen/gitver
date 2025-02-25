// @ts-check
import { defineConfig } from 'astro/config';

import db from '@astrojs/db';
import node from '@astrojs/node';

import tailwind from '@astrojs/tailwind';

// https://astro.build/config
export default defineConfig({
  integrations: [db(), tailwind()],
  output: 'server',
  adapter: node({
    mode: 'standalone',
  }),
});