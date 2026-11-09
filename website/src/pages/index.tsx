import React from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import styles from './index.module.css';
const session = [
 ['You','sessionText',"Find the Ujjayi breathing story in last week's class and get a clip ready."],
 ['Agent','sessionCmd','vidyom original search "Ujjayi breathing" --json'],
 ['','sessionOut','{"items": [{"asset_id":"ORI-001", "start_seconds":724}]}'],
 ['','sessionCmd','vidyom clip create --source ORI-001 --prompt "Explain Ujjayi breathing" --json'],
 ['','sessionOut','{"job_id":"JOB-002", "status":"queued"}'],
 ['','sessionCmd','vidyom approval list --json'],
 ['','sessionOut','{"items": [{"id":"APR-001", "kind":"clip_selection"}]}'],
 ['Agent','sessionText','Found the full story, 00:12:04 to 00:15:32. The preview is ready, and I need your approval before rendering the master.'],
 ['You','sessionCmd','vidyom approval approve APR-001'],
];
const rooms = [
 ['▦','The library','Originals, working masters and proxies live in storage you choose, with metadata and transcripts in open formats.'],
 ['◈','The logging desk','Every recording is transcribed, split into chapters and indexed, so an agent can search by meaning instead of watching hours of footage.'],
 ['✧','The cutting room','The agent describes a moment in plain language and gets back complete story candidates with timestamps, the transcript excerpt and a confidence score.'],
 ['◇','Finishing','Clips are rendered from the production master, then prepared with titles, descriptions, captions and thumbnails.'],
 ['⇄','Distribution','Publishes to YouTube, Vimeo, PeerTube and Instagram Reels through connectors. Approved videos also reach a searchable public library.'],
 ['⌘','The control room','Long jobs run on a persistent daemon, resume from checkpoints and can be inspected at any time.'],
];
const traits = [
 ['No timeline to click','Every action is a CLI command or a versioned JSON API call. No GUI is required at any step.'],
 ['Output it can parse','JSON output, predictable exit codes, and structured job states and error codes, so the agent never scrapes a terminal.'],
 ['Jobs that outlive the session','Submit a job, get an ID and disconnect. A 10 GB transcode keeps running, and the agent checks back later.'],
 ['Retries that do not double-post','Idempotency keys and declared side-effect classes keep a retry from uploading the same video twice.'],
 ['Workflows it can write','Workflows are versioned JSON. An agent can compose one from existing nodes, validate it and dry-run it before anything happens.'],
 ['Provider keys it never sees','Remote access uses a revocable per-device credential. Provider secrets stay in the server vault and never reach job payloads or logs.'],
];
const gates = [
 ['Every clip selection','An editor approves each proposed clip. No policy can skip this step.'],
 ['Every publication','A publisher or admin approves what goes public, by default before the upload starts.'],
 ['Every unsafe step','A step that cannot be retried safely is never retried automatically. The job waits for a person.'],
 ['Your originals','The raw original stays in the archive. Clips are rendered from the working master as new assets, with lineage back to their source.'],
 ['Your data','AI processing is local-only by default. An admin has to allow an external provider, and that consent is recorded.'],
];
export default function Home(){return <Layout title="The video editing studio run by AI agents" description="Vidyom is an open-source video editing studio run by AI agents: they find the stories in raw footage, cut clips and prepare them to publish, while people keep the final cut."><main>
<section className="heroV"><div className="container"><div className="row row--align-center"><div className="col col--6"><div className="heroEyebrow">OPEN SOURCE · BUILT FOR AI AGENTS</div><h1>A video editing studio run by your <span>AI agent.</span></h1><p>Hand your agent hours of raw footage and a brief. Vidyom gives it the tools of a full studio: a searchable library, story finding, rendering and publishing. The agent does the work. You keep the final cut.</p><div className="heroActions"><Link className="buttonPrimaryV" to="/docs/intro">Get started →</Link><Link className="buttonSecondaryV" href="https://github.com/rafaelp/vidyom">View on GitHub</Link></div></div><div className="col col--6 heroSession"><figure className="agentSession"><figcaption className="sessionBar"><span>Agent session</span><span>Example</span></figcaption><div className="sessionBody">{session.map(([role,kind,text])=><React.Fragment key={text}><span className={role==='Agent'?'sessionRole sessionRoleAgent':'sessionRole'}>{role}</span><span className={kind}>{kind==='sessionCmd'&&<span className="prompt">$ </span>}{text}</span></React.Fragment>)}</div></figure></div></div></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">Your agent is the editor. Vidyom is the studio.</h2><p className="sectionLead">Agents are good at understanding a brief. They are not built to store 10 GB recordings, transcribe them, render masters or reconcile uploads. Vidyom handles all of that and exposes each step as a command or API call.</p><div className="featureGrid">{rooms.map(([icon,title,description])=><div className="featureCard" key={title}><div className="featureIcon">{icon}</div><h3>{title}</h3><p>{description}</p></div>)}</div></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">Designed for an agent at the controls</h2><p className="sectionLead">Most video tools assume a person at a timeline. Vidyom assumes a client that reads structured output, checks back on long jobs and retries when something fails.</p><div className="traitGrid">{traits.map(([title,description])=><div className="trait" key={title}><h3>{title}</h3><p>{description}</p></div>)}</div><p className="statusNote">Agents connect through the CLI or the authenticated HTTPS API. <Link to="/docs/reference/cli">See the CLI reference →</Link></p></div></section>
<section className="finalCutBand"><div className="container"><div className="row"><div className="col col--5"><h2>The agent proposes. You keep the final cut.</h2><p>The agent does the work between the gates. At each gate the job stops and waits for a person, with no client needing to stay connected.</p></div><div className="col col--7"><ul className="gateList">{gates.map(([title,description])=><li className="gate" key={title}><strong>{title}</strong><span>{description}</span></li>)}</ul></div></div></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">Ingest once. Cut new clips whenever you need them.</h2><p className="sectionLead">An agent runs ingestion once per recording. It can come back months later and cut new clips from the same library, without touching the archive.</p><img className="workflowsImage" src={useBaseUrl('/img/workflows-overview.webp')} width={1774} height={887} loading="lazy" alt="Overview of the two Vidyom workflows: original ingestion builds a searchable library, and clip creation finds key moments, renders new videos and publishes them across channels." /><p className="statusNote">Overview of the two core workflows. <Link to="/docs/workflows/ingest-original">See the stages →</Link></p></div></section>
<section className="featureSection"><div className="container"><h2 className="sectionHeading">Run the studio on your own hardware</h2><p className="sectionLead">Vidyom is open source under Apache-2.0 and runs on a single Linux, macOS or Windows host with Docker Compose. Bring your own storage, AI endpoints and agent.</p><div className="heroActions"><Link className="buttonPrimaryV" to="/docs/intro">Get started →</Link><Link to="/docs/architecture/overview">See the architecture</Link></div></div></section>
</main></Layout>}
