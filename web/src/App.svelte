<script>
  import { route, link } from './lib/router.svelte.js'
  import Home from './pages/Home.svelte'
  import Ride from './pages/Ride.svelte'
</script>

<header class="wrap">
  <a class="brand" href="/" onclick={link}>
    <img src="/kiko.png" alt="" width="36" height="36" />
    <span class="mono">kiko<span class="dim">://</span></span>
  </a>
  <nav>
    <a href="/" onclick={link} class:active={route.path === '/'}>Home</a>
    <a class="btn" href="/ride" onclick={link}>Order a ride</a>
  </nav>
</header>

<main>
  {#if route.path === '/ride'}
    <Ride />
  {:else}
    <Home />
  {/if}
</main>

<footer class="wrap mono">
  <span>Don't trust. Verify.</span>
  <span class="dim">2-of-3 · x402 · cardano + bsv</span>
</footer>

<style>
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 1.25rem 0;
  }

  .brand {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    font-size: 1.15rem;
    font-weight: 700;
    color: var(--text);
  }

  .dim {
    color: var(--muted);
  }

  nav {
    display: flex;
    align-items: center;
    gap: 1.25rem;
  }

  nav a:not(.btn) {
    font: 0.9rem var(--mono);
    color: var(--muted);
  }

  nav a.active,
  nav a:not(.btn):hover {
    color: var(--teal-pale);
  }

  main {
    min-height: calc(100vh - 160px);
  }

  footer {
    display: flex;
    flex-wrap: wrap;
    justify-content: space-between;
    gap: 0.5rem;
    padding: 2.5rem 0;
    margin-top: 4rem;
    border-top: 1px solid var(--line);
    font-size: 0.8rem;
    color: var(--teal-soft);
  }

  @media (max-width: 520px) {
    nav a:not(.btn) {
      display: none;
    }
  }
</style>
