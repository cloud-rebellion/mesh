// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
'use strict';
const {performance}=require('node:perf_hooks');
const {Controller}=require('./controller.cjs');
const TIMING=Object.freeze({startup:30000,interval:6*60*60*1000,settle:5000,defer:60000,retry:Object.freeze([60000,300000,900000,3600000])});
const NATIVE_LOCKED=new Set(['quiescing','staging','ready','uncertain']);
class UpdateScheduler {
  // This owns deadlines, not channel authority. Production still receives the
  // disabled configureUpdater(null); only an accepted provider can enable it.
  constructor({controller,confirmEditor,now=()=>performance.now(),setTimer=setTimeout,clearTimer=clearTimeout,timing=TIMING}){
    if(!(controller instanceof Controller)||typeof confirmEditor!=='function')throw new Error('INVALID_UPDATE_SCHEDULER');
    for(const key of ['startup','interval','settle','defer'])if(!Number.isSafeInteger(timing[key])||timing[key]<1||timing[key]>86400000)throw new Error('INVALID_UPDATE_SCHEDULER');
    if(!Array.isArray(timing.retry)||!timing.retry.length||timing.retry.length>8||timing.retry.some(value=>!Number.isSafeInteger(value)||value<1||value>timing.interval))throw new Error('INVALID_UPDATE_SCHEDULER');
    Object.assign(this,{controller,confirmEditor,now,setTimer,clearTimer});this.timing={...timing,retry:[...timing.retry]};
    this.running=false;this.paused=false;this.generation=0;this.timer=null;this.deadline=null;this.flight=null;this.discovery=null;this.failures=0;this.halted=false;this.nextCheck=Infinity;this.nextInstall=Infinity;this.checkNotBefore=0;this.installNotBefore=0;
    this.onChange=()=>{if(this.state()==='available'&&!Number.isFinite(this.nextInstall))this.nextInstall=this.now()+this.timing.settle;this.plan();};
  }
  state(){return this.controller.updater.status().state;}
  start(){
    if(this.running||this.state()==='disabled')return false;
    this.running=true;this.paused=false;this.generation++;this.nextCheck=this.now()+this.timing.startup;
    if(this.state()==='available')this.nextInstall=this.nextCheck;
    this.controller.on('change',this.onChange);this.plan();return true;
  }
  clear(){if(this.timer!==null)this.clearTimer(this.timer);this.timer=null;this.deadline=null;}
  plan(){
    if(!this.running||this.paused||this.flight||this.halted||this.controller.quitLock||this.controller.updateLock||this.state()==='disabled'||NATIVE_LOCKED.has(this.state())){this.clear();return;}
    const deadline=this.state()==='available'?this.nextInstall:this.state()==='checking'?this.now()+this.timing.defer:this.nextCheck;
    if(!Number.isFinite(deadline)){this.clear();return;}
    if(this.timer!==null&&this.deadline===deadline)return;
    this.clear();this.deadline=deadline;
    this.timer=this.setTimer(()=>{this.timer=null;this.deadline=null;this.tick();},Math.max(0,Math.min(86400000,deadline-this.now())));this.timer?.unref?.();
  }
  wake(){
    if(!this.running||this.paused||this.halted)return;
    // Resume/editor events coalesce into one deadline, never a catch-up queue.
    if(this.state()==='available')this.nextInstall=Math.min(this.nextInstall,Math.max(this.installNotBefore,this.now()+this.timing.settle));
    else this.nextCheck=Math.min(this.nextCheck,Math.max(this.checkNotBefore,this.now()+this.timing.settle));
    this.plan();
  }
  tick(){
    if(!this.running||this.paused||this.flight||this.halted||this.state()==='disabled'||NATIVE_LOCKED.has(this.state())||this.controller.updateLock||this.controller.quitLock){this.plan();return this.flight||Promise.resolve();}
    const generation=this.generation;
    const work=Promise.resolve().then(()=>this.run(generation));this.flight=work;
    work.catch(()=>{this.halted=true;}).finally(()=>{if(this.flight===work)this.flight=null;this.plan();});return work;
  }
  current(generation){return this.running&&!this.paused&&this.generation===generation;}
  async run(generation){
    if(!this.current(generation))return;
    if(this.state()==='available'){
      if(this.now()<this.nextInstall)return;
      try{
        await this.controller.restartForUpdate({automatic:true,confirmEditor:async token=>{
          if(!this.current(generation))return false;
          const confirmed=await this.confirmEditor(token);
          return this.current(generation)&&confirmed===true;
        }});
      }catch(error){
        // Once native staging is uncertain, neither cancellation nor a timer
        // may reset it. An uncertain owner exit also needs human inspection.
        if(this.controller.updateLock||NATIVE_LOCKED.has(this.state())||error.message==='ENGINE_EXIT_UNCERTAIN')this.halted=true;
        else this.nextInstall=this.installNotBefore=this.now()+this.timing.defer;
      }
      return;
    }
    if(this.state()==='checking'){this.nextCheck=this.now()+this.timing.defer;return;}
    if(this.now()<this.nextCheck)return;
    const abort=new AbortController();this.discovery=abort;this.checkNotBefore=this.now()+this.timing.defer;
    try{
      await this.controller.updater.check({signal:abort.signal});
      if(this.current(generation)){this.failures=0;this.checkNotBefore=this.now()+this.timing.defer;this.nextCheck=this.now()+this.timing.interval;this.nextInstall=this.now()+this.timing.settle;this.controller.changed();}
    }catch(error){
      if(this.current(generation)&&!abort.signal.aborted){const index=Math.min(this.failures++,this.timing.retry.length-1);this.nextCheck=this.checkNotBefore=this.now()+this.timing.retry[index];this.controller.changed();}
    }finally{if(this.discovery===abort)this.discovery=null;}
  }
  pause(){
    this.paused=true;this.generation++;this.clear();this.discovery?.abort();
    // Existing updateLock owns a native restart. Ordinary quit refuses that
    // transition; this cannot cancel/release a staged or uncertain update.
    return this.flight?this.flight.catch(()=>{}):Promise.resolve();
  }
  resume(){if(!this.running)return;this.paused=false;this.generation++;this.nextCheck=Math.max(this.checkNotBefore,Math.min(this.nextCheck,this.now()+this.timing.settle));this.plan();}
  async stop(){
    const completion=this.pause();this.running=false;this.controller.removeListener('change',this.onChange);await completion;
    if(!this.controller.updateLock&&!NATIVE_LOCKED.has(this.state()))await this.controller.updater.dispose?.();
  }
}
module.exports={UpdateScheduler,TIMING};
