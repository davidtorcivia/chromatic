import { afterEach, describe, expect, it } from 'vitest';

import {
	activeReviewToolCount,
	degradedInterval,
	getReviewQualityMode,
	loadSnapshot,
	setReviewQualityMode,
	setReviewToolActive,
	stopLoadMonitor,
} from './loadMonitor';

function clearTools() {
	for (const tool of ['laser', 'loupe', 'scopes'] as const) {
		for (let i = 0; i < 5; i++) setReviewToolActive(tool, false);
	}
	setReviewQualityMode('balanced');
}

describe('loadMonitor review tool pressure', () => {
	afterEach(clearTools);

	it('reference-counts mounted review tools by category', () => {
		clearTools();

		setReviewToolActive('laser', true);
		setReviewToolActive('laser', true);
		expect(activeReviewToolCount()).toBe(1);

		setReviewToolActive('laser', false);
		expect(activeReviewToolCount()).toBe(1);

		setReviewToolActive('laser', false);
		expect(activeReviewToolCount()).toBe(0);
	});

	it('backs off lower-priority tool work when multiple review tools are active', () => {
		clearTools();

		expect(degradedInterval('loupe', 32)).toBe(32);

		setReviewToolActive('laser', true);
		setReviewToolActive('loupe', true);

		expect(degradedInterval('loupe', 32)).toBeCloseTo(44.8);
		expect(degradedInterval('scopes', 42)).toBeCloseTo(75.6);

		setReviewToolActive('scopes', true);

		expect(degradedInterval('loupe', 32)).toBeCloseTo(62.72);
		expect(degradedInterval('scopes', 42)).toBeCloseTo(136.08);
	});

	it('applies review quality mode multipliers', () => {
		clearTools();

		setReviewQualityMode('performance');
		expect(getReviewQualityMode()).toBe('performance');
		expect(degradedInterval('loupe', 32)).toBeCloseTo(51.2);
		expect(degradedInterval('scopes', 42)).toBeCloseTo(92.4);

		setReviewQualityMode('fidelity');
		expect(degradedInterval('loupe', 32)).toBeCloseTo(27.2);
		expect(degradedInterval('scopes', 42)).toBeCloseTo(35.7);
	});

	it('exposes a load snapshot for diagnostics', () => {
		clearTools();
		setReviewQualityMode('performance');
		setReviewToolActive('laser', true);

		expect(loadSnapshot()).toMatchObject({
			activeReviewToolCount: 1,
			qualityMode: 'performance',
			underPressure: false,
		});
	});

	it('resets session-scoped tool state when the monitor stops', () => {
		clearTools();

		setReviewToolActive('laser', true);
		setReviewToolActive('loupe', true);
		expect(activeReviewToolCount()).toBe(2);

		stopLoadMonitor();

		expect(loadSnapshot()).toMatchObject({
			activeReviewToolCount: 0,
			longFrameCount: 0,
			worstLongFrameMs: null,
		});
	});
});
