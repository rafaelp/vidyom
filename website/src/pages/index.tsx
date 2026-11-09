import React from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import styles from './index.module.css';
const features = [
 ['◈','Workflow-native','Design repeatable video pipelines in JSON, inspired by n8n. No code required to compose existing nodes.'],
 ['⌘','CLI + daemon','Control persistent background jobs locally or securely across your private network.'],
 ['✧','AI-assisted clipping','Find complete stories from natural-language prompts and render independent derivative assets.'],
 ['▦','Portable media archive','Keep canonical assets, metadata, captions, and lineage independent of platforms.'],
 ['⇄','Connector ecosystem','Use pluggable storage, publishing, AI, catalog, and observability providers.'],
 ['◇','Secure by design','Keyring-backed secrets, private networking, policy-aware plugins, and auditable jobs.'],
];
export default function Home(){return <Layout title="Open-source AI video workflows" description="Documentation and architecture for the Vidyom video workflow engine"><main>
<section className="heroV"><div className="container"><div className="row"><div className="col col--7"><div className="heroEyebrow">OPEN SOURCE · VIDEO AUTOMATION · KNOWLEDGE</div><h1>Meet <span>Vidyom.</span></h1><p>Preserve originals. Find meaningful moments. Create new videos. Publish everywhere — with a programmable, extensible workflow engine.</p><div className="heroActions"><Link className="buttonPrimaryV" to="/docs/intro">Explore the documentation →</Link><Link className="buttonSecondaryV" to="/docs/architecture/overview">View architecture</Link></div><div className="statusNote">Architecture and implementation specification · Not yet a production release</div></div><div className="col col--5 heroGraphic"><div className="orbit"><img className="orbitMark" src={useBaseUrl('/img/vidyom-mark.png')} alt="" /></div></div></div></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">Designed for an open media ecosystem</h2><div className="featureGrid">{features.map(([icon,title,description])=><div className="featureCard" key={title}><div className="featureIcon">{icon}</div><h3>{title}</h3><p>{description}</p></div>)}</div></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">One library. Unlimited possibilities.</h2><div className="terminalDemo"><span className="prompt">$</span> vidyom original import yoga-class.mp4 --workflow ingest-original{'\n'}<span className="prompt">$</span> vidyom clip create --source ORI-001 --prompt "Explain Ujjayi breathing"{'\n'}<span className="prompt">$</span> vidyom job logs JOB-001 --follow</div><p className="statusNote">Illustrative proposed commands. The CLI and daemon are specified but not implemented in this documentation package.</p></div></section>
</main></Layout>}
