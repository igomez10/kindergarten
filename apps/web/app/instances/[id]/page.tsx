import InstanceDetail from "./detail";

// The export contains one shell page, /instances/_.html; the Go server serves
// it for every /instances/<id>, and the client reads the ID from the URL.
export function generateStaticParams() {
  return [{ id: "_" }];
}

export default function Page() {
  return <InstanceDetail />;
}
