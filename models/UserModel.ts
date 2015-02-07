'use strict';
class User {
  private _id:number;
  private _name:string;
  private _newName:string;


  constructor(id:number, name:string) {
    this._id = id;
    this._name = name;
  }

  public get Name():string {
    return this._name;
  }

  public set Name(newName:string) {
    this._name = newName;
  }

  public get NewName():string {
    return this._newName;
  }

  public set NewName(newName:string) {
    this._newName = newName;
  }

  public get Id():number {
    return this._id;
  }

  public set Id(newId:number) {
    this._id = newId;
  }
}
export = User;