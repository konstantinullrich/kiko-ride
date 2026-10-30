<script>
  import { search } from './api.js'

  // value is the address text; point the chosen place's coordinates, cleared
  // whenever the text changes so a stale point is never sent.
  let { label, placeholder = '', value = $bindable(''), point = $bindable(null) } = $props()

  let results = $state([])
  let open = $state(false)
  let error = $state('')
  let timer

  function input() {
    point = null
    error = ''
    clearTimeout(timer)
    if (value.trim().length < 3) {
      results = []
      return
    }
    timer = setTimeout(async () => {
      try {
        results = await search(value)
        open = true
      } catch (e) {
        error = e.message
      }
    }, 300)
  }

  function pick(place) {
    value = place.address
    point = { lat: place.lat, lng: place.lng }
    open = false
  }
</script>

<div class="field">
  <label>
    {label}
    <input
      type="text"
      autocomplete="off"
      {placeholder}
      bind:value
      oninput={input}
      onfocus={() => (open = results.length > 0)}
      onblur={() => setTimeout(() => (open = false), 150)}
    />
  </label>
  {#if point}
    <span class="pin mono">⌖ {point.lat.toFixed(4)}, {point.lng.toFixed(4)}</span>
  {/if}
  {#if open && results.length}
    <ul class="panel">
      {#each results as place}
        <li><button type="button" onclick={() => pick(place)}>{place.address}</button></li>
      {/each}
    </ul>
  {/if}
  {#if error}<p class="hint">{error}</p>{/if}
</div>

<style>
  .field {
    position: relative;
  }

  label input {
    margin-top: 0.4rem;
    text-transform: none;
    letter-spacing: normal;
  }

  .pin {
    position: absolute;
    right: 0.75rem;
    top: 2.3rem;
    font-size: 0.72rem;
    color: var(--teal-soft);
    pointer-events: none;
  }

  ul {
    position: absolute;
    z-index: 5;
    left: 0;
    right: 0;
    margin: 0.35rem 0 0;
    padding: 0.35rem;
    list-style: none;
    background: var(--panel);
  }

  button {
    width: 100%;
    text-align: left;
    background: none;
    border: 0;
    border-radius: 8px;
    color: var(--text);
    font: 0.9rem var(--sans);
    padding: 0.55rem 0.7rem;
    cursor: pointer;
  }

  button:hover {
    background: var(--panel-2);
    color: var(--teal-pale);
  }
</style>
