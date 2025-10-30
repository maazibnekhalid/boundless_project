import React, { useEffect, useState } from "react";

const API = (path) =>
  `${window.location.protocol}//${window.location.hostname}:${window.location.port || 5173}${path}`;

async function fetchJSON(path) {
  const url =
    import.meta.env.PROD
      ? `/api${path.replace(/^\/api/, "")}`
      : `http://localhost:8080${path}`;
  const r = await fetch(url);
  if (!r.ok) throw new Error(`fetch ${path}: ${r.status}`);
  return r.json();
}

function CopyButton({ text }) {
  return (
    <button
      onClick={() => navigator.clipboard.writeText(text)}
      className="px-3 py-1 rounded-lg bg-gray-800 text-white text-sm hover:bg-gray-700"
    >
      Copy
    </button>
  );
}

export default function App() {
  const [snapshots, setSnapshots] = useState([]);
  const [collateral, setCollateral] = useState([]);
  const [err, setErr] = useState("");

  useEffect(() => {
    (async () => {
      try {
        const [s, c] = await Promise.all([
          fetchJSON("/api/snapshots"),
          fetchJSON("/api/collateral"),
        ]);
        setSnapshots(s);
        setCollateral(c);
      } catch (e) {
        setErr(String(e));
      }
    })();
  }, []);

  const snapCsv = [
    ["bucket", "captured_at", "label", "addr", "orders_taken", "cycles_proved"],
    ...snapshots.map((r) => [
      r.bucket,
      r.captured_at,
      r.label,
      r.addr,
      r.orders_taken,
      r.cycles_proved,
    ]),
  ]
    .map((row) => row.join(","))
    .join("\n");

  const colCsv = [
    ["captured_at", "label", "addr", "balance_wei", "balance_eth"],
    ...collateral.map((r) => [
      r.captured_at,
      r.label,
      r.addr,
      r.balance_wei,
      r.balance_eth,
    ]),
  ]
    .map((row) => row.join(","))
    .join("\n");

  return (
    <div className="max-w-6xl mx-auto p-6 space-y-10">
      <header className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Boundless Dashboard</h1>
        {err && <div className="text-red-600">{err}</div>}
      </header>

      <section className="bg-white shadow rounded-2xl p-5">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-lg font-medium">2-Hour Snapshots (UTC-aligned)</h2>
          <CopyButton text={snapCsv} />
        </div>
        <div className="overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead className="bg-gray-100">
              <tr>
                <th className="p-2 text-left">Bucket</th>
                <th className="p-2 text-left">Captured</th>
                <th className="p-2 text-left">Label</th>
                <th className="p-2 text-left">Addr</th>
                <th className="p-2 text-right">Orders</th>
                <th className="p-2 text-right">Cycles</th>
              </tr>
            </thead>
            <tbody>
              {snapshots.map((r, i) => (
                <tr key={i} className="border-b last:border-0">
                  <td className="p-2">{r.bucket}</td>
                  <td className="p-2">{r.captured_at}</td>
                  <td className="p-2">{r.label}</td>
                  <td className="p-2">{r.addr}</td>
                  <td className="p-2 text-right">{r.orders_taken}</td>
                  <td className="p-2 text-right">{Math.round(r.cycles_proved)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="bg-white shadow rounded-2xl p-5">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-lg font-medium">Collateral (15-min)</h2>
          <CopyButton text={colCsv} />
        </div>
        <div className="overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead className="bg-gray-100">
              <tr>
                <th className="p-2 text-left">Captured</th>
                <th className="p-2 text-left">Label</th>
                <th className="p-2 text-left">Addr</th>
                <th className="p-2 text-right">Balance (wei)</th>
                <th className="p-2 text-right">Balance (eth)</th>
              </tr>
            </thead>
            <tbody>
              {collateral.map((r, i) => (
                <tr key={i} className="border-b last:border-0">
                  <td className="p-2">{r.captured_at}</td>
                  <td className="p-2">{r.label}</td>
                  <td className="p-2">{r.addr}</td>
                  <td className="p-2 text-right">{r.balance_wei}</td>
                  <td className="p-2 text-right">{r.balance_eth}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}
