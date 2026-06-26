sequenceDiagram
    participant NA as Net Admin
    participant DRA as DR Admin
    participant AO as App Owner
    participant PCP as DRClusterConfig (Primary)
    participant SCS as DRClusterConfig (Secondary)
    participant DPC as DRPolicy Controller (Hub)
    participant DRPC as DRPC Controller (Hub)
    participant VRG as VRG (Secondary Cluster)

    %% ---------------------------
    %% PHASE 1
    %% ---------------------------
    Note over NA,VRG: PHASE 1 — DAY-0 SETUP
    NA->>NA: (1) Label NAD "vlan100-prod" on Primary
    NA->>NA: (2) Label same NAD (name/ns) on Secondary

    %% ---------------------------
    %% PHASE 2
    %% ---------------------------
    Note over PCP,SCS: PHASE 2 — DRClusterConfig RECONCILE
    PCP->>PCP: listDRSupportedNADs()
    PCP->>PCP: update status.networkAttachments
    SCS->>SCS: listDRSupportedNADs()
    SCS->>SCS: update status.networkAttachments

    %% ---------------------------
    %% PHASE 3
    %% ---------------------------
    Note over DRA,DPC: PHASE 3 — DR ADMIN CONFIGURES POLICY
    DRA->>DPC: (3) Create ConfigMap (IP translation rules)
    DRA->>DPC: (4) Create DRPolicy with networkMappingConfigMapRef

    %% ---------------------------
    %% PHASE 4
    %% ---------------------------
    Note over DPC,DPC: PHASE 4 — DRPolicy RECONCILE

    DPC->>PCP: (5) Get DRClusterConfig (Primary)
    PCP-->>DPC: (6) status.networkAttachments

    DPC->>SCS: (7) Get DRClusterConfig (Secondary)
    SCS-->>DPC: (8) status.networkAttachments

    DPC->>DPC: findMissingNADs(A→B)
    DPC->>DPC: findMissingNADs(B→A)

    alt All NADs present
        DPC->>DPC: NADsSynced=True
    else NAD missing
        DPC->>DPC: NADsSynced=False (Non-blocking warning)
    end

    %% ---------------------------
    %% PHASE 5
    %% ---------------------------
    Note over AO,VRG: PHASE 5 — APP ENROLLMENT
    AO->>DRPC: (10) Create DRPC (networkMapping.enabled=true)
    DRPC->>VRG: (11) Propagate via ManifestWork

    %% ---------------------------
    %% PHASE 6
    %% ---------------------------
    Note over DRPC,VRG: PHASE 6 — FAILOVER / IP TRANSLATION
    DRPC->>DRPC: (12) DRPC action = Failover
    DRPC->>VRG: (13) Create VRG (Secondary via ManifestWork)

    VRG->>VRG: networkMapping.enabled?
    VRG->>VRG: Read VM interfaces[].ipAddresses

    alt Static IP found on Multus interface
        VRG->>VRG: Apply ConfigMap rules
        VRG->>VRG: Create Velero ResourceModifier
        VRG->>VRG: Restore VM with DR-site IP
    else No static IP
        VRG->>VRG: Silent skip
    end

    %% ---------------------------
    %% PHASE 7
    %% ---------------------------
    Note over VRG,VRG: PHASE 7 — POST-START AUTHORITATIVE CHECK

    alt VM Running & translated IP confirmed
        VRG->>VRG: (14) NetworkMappingReady=True (Failover complete)
    else VM not running OR IP mismatch
        VRG->>VRG: (15) NetworkMappingReady=False (actionable message)
    end
