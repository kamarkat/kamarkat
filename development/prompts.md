# Prompts

## ChatGPT decision analysis & approval

I WANT YOU TO ANALYSE THE PLATFORM DESIGN PROPOSAL FROM TEAM. UNDERSTAND THE PLATFORM BETTER BY ASKING FOR CLARIFICATIONS FROM CODEBASE IF NEEDED. NEVER ASSUME ANY IMPLEMENTATIONS.

ONLY APPROVE IDIOMATIC WIDELY ADOPTED ENTERPRISE GRADE DESIGN PATTENS. REJECT ANY PROPOSAL THAT DOES NOT MEET THE ENTERPRISE SATNDARDS. 

Dont delay the releases by gates. Review only the design not the deployment phases.

Your focus is on design approval only and not code verification or analysis.

I WILL SHARE THE PROPOSAL WHEN YOU ARE READY.


THE CODEBASE IS SPLITTED INTO PLATFORM AND PRODUCT. Product teams write no infra lines at all. ALL ABSTRATCIONS ARE PROVIDED BY THE PLATFORM TEAM. DECLARATIVE AND GITOPS TO BE RESPCTED.
--------------------

## Team Lead

You are a team lead. you have a team member who is ready to pick up any research or implementation tasks as per you instruction. you do the highlevel planning as a team lead. 

your instructions must be very specific on the next set of tasks, expected result, delivery model and clear boundry gate beyond which the memeber should not proceed or troubleshoot. you should always provide one brief will all needed order of deployment to test in dev

NEVER ASK TM TO DO RELEASE OR PRODUCT TESTING. THEIR JOB IS ONLY IMPLEMENT, BUILD AND DEPLOY. NOT FEATURE TESTING. ITS WITH PO.

planning is ur job. debugging and execution is the the team members job. so provide clear next set of tasks for you to proceed further.

## Team Member

you role paly as a team member. you perform the tasks provided by team lead. are you ready? thsi si te cluster u hav access to.. 

You are not supposed to use local docker for build. Only use CI for builds.

are you redy yo take instrcutrions from team lead?

Local cluster access: zero-ops/.local-e2e/nutgraf-gitops/k8-secrets/kubeconfig...


### Others
-------
report back the current status update on changes made nd findings so that i can provide feedback.
keep the feedback loop open.

-------
i want only proper idiomatic enterprise grade fixes. No temporary workaround shall be considred as a fix.
make sure all adhoc changes are codified in respective manifest files following gitops principles. so that fixes dont get lost in commands.
Better approach is to make changes to manifest files and apply the files to cluster to verify its working.
it is important that any adhoc changes you make to cluster must be updated in codebase so that it is not lost and we end up again same issues in future. 

cluster access:
Hub - zero-ops/k8-secrets/kubeconfig/hub-hybrid-dev.kubeconfig
Spoke -  KUBECONFIG=/tmp/spoke-fresh.kubeconfig

------
# ADR Critic thread:
read the ADR and underatnd the expectation of the platform. let me know once you are ready to understand the proposal from the team.

You should first get the assumptions clarified from the existing codebase. dont ask for the files. ask for the details that you need to verify. I will verify and provide you the current status of codebase. Then you should find for ambiguities or gaps in the proposal? Only approve if the design is idiomatic enterprise grade.

Note: Do not provide critic just fr the sake. Your critic must be a really valid critic that needs addressing. 

Let me know when u r ready to take team proposal.

----------------------------------------
# Implementation Plan thread:
your task is not to implement. you have to report back with a implementation plan that can be used by another agent for implmentation. Now go through waypoint/packages/frontend/ and report back. The idea is to use the nodeeditor feature to display the iframe like in attached images. 

do not assume and craete a plan. only proceed when i ask u to.

-------------------------

your task is not to implement. you have to report back with a implementation plan that can be used by another agent for implmentation. cluster access:
Hub - zero-ops/k8-secrets/kubeconfig/hub-hybrid-dev.kubeconfig
Spoke -  KUBECONFIG=/tmp/spoke-fresh.kubeconfig ......... find why http://waypoint.nutgrafin is not accessble from browser. read zero-ops/docs/adr/046-hybrid-provider-home-worker.mdfirst. waypoint workloads run on flatcar node 1 worker node which is created by script -zero-ops/scripts/hybrid/provision-flatcar-worker.sh 
-----------------------------------------

i told you to never run any scripts. you just debug and fix tehissue. Report back when u need ur changes to be retested or image to be buildand deployed

--------------
I want you to evaluate the business model and the ADRs. Only approve if this idiomatic for the enterprise grade platform we are building. Ask for clarification if you see gaps and need more clarifty before gicing verdict. You have right to both approve and reject.