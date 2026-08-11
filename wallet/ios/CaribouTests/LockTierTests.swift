import XCTest
// LockTier.swift is compiled directly into this logic-test bundle (see
// project.yml), so the tier logic runs hermetically — no app host, no gomobile
// Mobile.xcframework, no simulator UI. No import of the app module is needed.

// LockTierTests exercises the pure session-lock tier decision (lockTier), the
// security-critical mapping from idle-time-backgrounded to which re-auth runs on
// resume. It mirrors the coverage of Android's instrumented BiometricAuth tests
// for the shared lock policy. Defaults used below match AppModel's: gate 10 min,
// eviction 1 hour.
final class LockTierTests: XCTestCase {
    private let gateMs = 10.0 * 60_000   // 10 min
    private let evictMs = 60.0 * 60_000  // 1 hour

    // A quick app switch (under the gate window) resumes untouched.
    func testUnderGateResumesUntouched() {
        XCTAssertEqual(lockTier(idleMs: 0, gateMs: gateMs, evictMs: evictMs, canGate: true), .none)
        XCTAssertEqual(lockTier(idleMs: gateMs - 1, gateMs: gateMs, evictMs: evictMs, canGate: true), .none)
    }

    // In the gate band with an auth mechanism: the cheap UI gate.
    func testGateBandRunsGate() {
        XCTAssertEqual(lockTier(idleMs: gateMs, gateMs: gateMs, evictMs: evictMs, canGate: true), .gate)
        XCTAssertEqual(lockTier(idleMs: evictMs - 1, gateMs: gateMs, evictMs: evictMs, canGate: true), .gate)
    }

    // Past the eviction window: full eviction, regardless of auth mechanism.
    func testPastEvictionEvicts() {
        XCTAssertEqual(lockTier(idleMs: evictMs, gateMs: gateMs, evictMs: evictMs, canGate: true), .evict)
        XCTAssertEqual(lockTier(idleMs: evictMs + 60_000, gateMs: gateMs, evictMs: evictMs, canGate: false), .evict)
    }

    // The gate band with NO auth mechanism escalates to eviction rather than leave
    // the authed session exposed past the gate window.
    func testGateBandWithoutAuthEscalatesToEviction() {
        XCTAssertEqual(lockTier(idleMs: gateMs, gateMs: gateMs, evictMs: evictMs, canGate: false), .evict)
        XCTAssertEqual(lockTier(idleMs: evictMs - 1, gateMs: gateMs, evictMs: evictMs, canGate: false), .evict)
    }

    // The boundaries are inclusive at the lower edge of each tier (>=), so a value
    // exactly at gateMs gates and exactly at evictMs evicts.
    func testBoundariesAreInclusiveLower() {
        XCTAssertEqual(lockTier(idleMs: gateMs, gateMs: gateMs, evictMs: evictMs, canGate: true), .gate)
        XCTAssertEqual(lockTier(idleMs: evictMs, gateMs: gateMs, evictMs: evictMs, canGate: true), .evict)
    }

    // gate 0 ("require unlock immediately") gates on any resume; a lack of auth
    // there still escalates to eviction.
    func testGateZeroRequiresUnlockImmediately() {
        XCTAssertEqual(lockTier(idleMs: 0, gateMs: 0, evictMs: evictMs, canGate: true), .gate)
        XCTAssertEqual(lockTier(idleMs: 1, gateMs: 0, evictMs: evictMs, canGate: false), .evict)
    }

    // Eviction set to Never (greatestFiniteMagnitude, AppModel's sentinel): the
    // gate band extends to infinity, so with an auth mechanism it only ever gates,
    // never evicts.
    func testEvictionNeverOnlyGates() {
        let never = Double.greatestFiniteMagnitude
        XCTAssertEqual(lockTier(idleMs: 24.0 * 60 * 60_000, gateMs: gateMs, evictMs: never, canGate: true), .gate)
        // ...but without an auth mechanism the gate band still escalates to eviction.
        XCTAssertEqual(lockTier(idleMs: gateMs, gateMs: gateMs, evictMs: never, canGate: false), .evict)
    }
}
