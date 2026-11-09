import { formatDateTime } from '@/lib/format';

export function SiteFooter({ generatedAt }: { generatedAt: string | null }) {
  const updated = generatedAt ? formatDateTime(generatedAt) : '';
  return (
    <footer className="site-footer">
      <div className="site-footer__inner">
        <p>
          {updated ? (
            <>
              Catalog updated <time dateTime={generatedAt ?? undefined}>{updated} UTC</time>
            </>
          ) : (
            'Public video library'
          )}
        </p>
        <p>
          Powered by <a href="https://www.vidyom.org">Vidyom</a>
        </p>
      </div>
    </footer>
  );
}
