import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [
    sveltekit({
      preprocess: vitePreprocess(),
      csp: { mode: 'hash' },
      adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html' })
    })
  ],
  server: {
    fs: { allow: ['..'] },
    proxy: { '/api': { target: 'http://127.0.0.1:8080', ws: true } }
  }
});
