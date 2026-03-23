// src/lib/api.js — Fetch wrapper for the HisnOS onboarding Go backend.
//
// All requests go to /api/* (same origin).  No auth tokens — the server
// binds only to 127.0.0.1 so only local processes can reach it.

const BASE = '';

async function request(method, path, body) {
	const opts = {
		method,
		headers: { 'Content-Type': 'application/json' },
	};
	if (body !== undefined) {
		opts.body = JSON.stringify(body);
	}
	const res = await fetch(BASE + path, opts);
	const json = await res.json();
	if (!res.ok) {
		throw new Error(json.error ?? `HTTP ${res.status}`);
	}
	return json;
}

// Returns the full onboarding state object.
export function getState() {
	return request('GET', '/api/state');
}

// Marks the welcome step complete.
export function postWelcome() {
	return request('POST', '/api/step/welcome', {});
}

// Initialises (or skips) the vault.
// opts: { passphrase?: string, skip?: boolean }
export function postVault(opts) {
	return request('POST', '/api/step/vault', opts);
}

// Selects a firewall profile.
// profile: 'strict' | 'balanced' | 'gaming-ready'
export function postFirewall(profile) {
	return request('POST', '/api/step/firewall', { profile });
}

// Configures threat notifications.
export function postThreat(notifications) {
	return request('POST', '/api/step/threat', { notifications });
}

// Opts in/out of the gaming group.
export function postGaming(enable) {
	return request('POST', '/api/step/gaming', { enable });
}

// Runs system verification checks.
export function getVerify() {
	return request('GET', '/api/verify');
}

// Marks the wizard complete.
export function postComplete() {
	return request('POST', '/api/complete', {});
}

// Skips a step with a reason.
export function postSkip(step, reason) {
	return request('POST', '/api/skip', { step, reason });
}
