<script>
  // Global layout — dark theme styles applied to the full page.
</script>

<slot />

<style>
  :global(*) {
    box-sizing: border-box;
    margin: 0;
    padding: 0;
  }

  :global(body) {
    font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
    background: #0a0a14;
    color: #ccd6f6;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  :global(button) {
    cursor: pointer;
    font-size: 0.95rem;
    font-family: inherit;
    border: none;
    border-radius: 6px;
    padding: 0.55rem 1.4rem;
    transition: background 0.15s, opacity 0.15s;
  }

  :global(button:disabled) {
    opacity: 0.45;
    cursor: not-allowed;
  }

  :global(.btn-primary) {
    background: #00c8ff;
    color: #0a0a14;
    font-weight: 600;
  }

  :global(.btn-primary:hover:not(:disabled)) {
    background: #33d4ff;
  }

  :global(.btn-secondary) {
    background: #1a2030;
    color: #8892b0;
    border: 1px solid #2a3a50;
  }

  :global(.btn-secondary:hover:not(:disabled)) {
    background: #202a40;
    color: #ccd6f6;
  }

  :global(.error-msg) {
    color: #ff6b6b;
    font-size: 0.875rem;
    margin-top: 0.5rem;
  }

  :global(.warning-msg) {
    color: #ffa94d;
    font-size: 0.875rem;
    margin-top: 0.5rem;
  }

  :global(input[type="password"],
          input[type="text"],
          input[type="email"]) {
    background: #0d1117;
    border: 1px solid #2a3a50;
    border-radius: 6px;
    color: #ccd6f6;
    font-size: 0.95rem;
    padding: 0.5rem 0.75rem;
    width: 100%;
    outline: none;
    transition: border-color 0.15s;
  }

  :global(input:focus) {
    border-color: #00c8ff;
  }
</style>
