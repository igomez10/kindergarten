import type { Metadata } from "next";
import "./globals.css";
import Providers from "./providers";
import Header from "./header";

export const metadata: Metadata = {
  title: "pgcloud",
  description: "Self-service PostgreSQL on the homelab",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Providers>
          <Header />
          <main>{children}</main>
        </Providers>
      </body>
    </html>
  );
}
