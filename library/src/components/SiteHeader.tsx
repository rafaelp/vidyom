import Link from 'next/link';
import { HeaderSearch } from './HeaderSearch';
import { ThemeToggle } from './ThemeToggle';

export function SiteHeader({ title }: { title: string }) {
  return (
    <header className="site-header">
      <div className="site-header__inner">
        <Link href="/" className="brand">
          {/* eslint-disable-next-line @next/next/no-img-element */}
          <img src="/vidyom-mark.png" alt="" width={32} height={32} />
          <span>{title}</span>
        </Link>
        <HeaderSearch />
        <div className="header-actions">
          <ThemeToggle />
        </div>
      </div>
    </header>
  );
}
