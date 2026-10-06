'use strict';
(() => {
  // Fill unobserved time explicitly: a restart is not evidence of an outage
  // or continuous uptime. Intervals are half-open, except the visible endpoint.
  function intervals(history, camera) {
    const result = [];
    let cursor = history.start;
    for (const interval of camera.intervals) {
      const start = Math.max(cursor, interval.start, history.start);
      const end = Math.min(interval.end, history.end);
      if (end <= start) continue;
      if (start > cursor) result.push({start: cursor, end: start, state: 'unknown'});
      result.push({start, end, state: interval.state});
      cursor = end;
    }
    if (cursor < history.end) result.push({start: cursor, end: history.end, state: 'unknown'});
    return result;
  }

  function at(intervals, value) {
    return intervals.find((interval, index) => value >= interval.start &&
      (value < interval.end || (index === intervals.length - 1 && value === interval.end)));
  }

  window.DozorAvailability = {intervals, at};
})();
