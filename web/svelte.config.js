import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

export default {
  preprocess: vitePreprocess(),
  kit: {
    csp: { mode: 'hash' },
    adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html' })
  }
};
