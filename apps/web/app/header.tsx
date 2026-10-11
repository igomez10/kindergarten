"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";

export default function Header() {
  const path = usePathname();
  const me = useQuery({ queryKey: ["me"], queryFn: api.me, staleTime: 60_000 });
  const link = (href: string, label: string) => (
    <Link href={href} className={path === href || (href !== "/" && path.startsWith(href + "/")) ? "nav active" : "nav"}>
      {label}
    </Link>
  );
  return (
    <header className="topbar">
      <Link href="/instances" className="brand">
        <span className="brand-mark" aria-hidden>◆</span> pgcloud
      </Link>
      <nav>
        {link("/instances", "Databases")}
        {link("/settings", "Settings")}
      </nav>
      <div className="who">
        {me.data && <span className="muted">{me.data.user.email || me.data.user.subject}</span>}
        <a href="/oauth2/sign_out?rd=%2F" className="nav">Sign out</a>
      </div>
    </header>
  );
}
